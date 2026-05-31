// Package main 实现 DramaMate 短剧出海的本地加工管道。
//
// 职责边界：DramaMate 只负责「把一条中文短剧加工成可发布的成片+元数据」，
// 上传/发布（OAuth2、平台 API）不在本项目范围，由上层调用方处理。
//
// 管道（单条视频）：
//
//	输入 MP4
//	  └─[1] ffmpeg 分离音频           -> audio.mp3
//	        └─[2] Whisper 转录(中文)   -> 中文 SRT
//	              └─[3] LLM 翻译(英文)  -> en.srt
//	                    └─[4] ffmpeg 硬烧录 + 竖屏 + 去重 -> output.mp4
//	                          └─[5] LLM 生成标题/标签       -> metadata.json
//
// 产出 output.mp4 + metadata.json 两个工件交给上层调用方消费。
//
// 设计原则：全程透传 context.Context（可取消/超时）；每一步显式 error
// 包裹（fmt.Errorf("...: %w", err)）；外部命令与 HTTP 均受 ctx 控制。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// 配置 & 全局常量
// ---------------------------------------------------------------------------

// Config 聚合管道运行所需的外部依赖配置。
// 生产环境应从环境变量 / Secret Manager 注入，切勿硬编码密钥。
type Config struct {
	// NewAPI（OpenAI 兼容转发层）相关。
	APIBaseURL string // 例如 https://newapi.example.com/v1
	APIKey     string // Bearer Token

	WhisperModel string // 例如 whisper-1
	LLMModel     string // 例如 claude-sonnet-4-6 / gpt-4o

	// 本地工作目录，存放中间产物（audio.mp3 / *.srt / output.mp4）。
	WorkDir string

	// ffmpeg 可执行文件路径（默认依赖 PATH）。
	FFmpegBin string

	// 画布模式：blur | pad | original | crop | caption | colorpad。
	VideoMode string

	// 画布相关可调参数（仅部分模式用到）。
	PadColor  string // pad/colorpad/caption 的填充色，如 black/white/#1e1e1e
	BlurSigma string // blur 模式高斯模糊强度，默认 20
	FgScale   string // blur 模式前景占画布宽度比例(0.5~1.0)，默认 1.0
}

// loadConfig 从环境变量构建配置，并填充合理默认值。
func loadConfig() (*Config, error) {
	cfg := &Config{
		APIBaseURL:   getenv("DRAMA_API_BASE_URL", "https://newapi.example.com/v1"),
		APIKey:       os.Getenv("DRAMA_API_KEY"),
		WhisperModel: getenv("DRAMA_WHISPER_MODEL", "whisper-1"),
		LLMModel:     getenv("DRAMA_LLM_MODEL", "claude-sonnet-4-6"),
		WorkDir:      getenv("DRAMA_WORK_DIR", "./work"),
		FFmpegBin:    getenv("DRAMA_FFMPEG_BIN", "ffmpeg"),
		VideoMode:    getenv("DRAMA_VIDEO_MODE", "blur"),
		PadColor:     getenv("DRAMA_PAD_COLOR", "black"),
		BlurSigma:    getenv("DRAMA_BLUR_SIGMA", "20"),
		FgScale:      getenv("DRAMA_FG_SCALE", "1.0"),
	}
	if cfg.APIKey == "" {
		// MVP 阶段允许为空（转录/翻译会走 Mock 分支），但生产环境必须校验。
		slog.Warn("DRAMA_API_KEY 未设置，转录/翻译将退化为 Mock 数据")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建工作目录失败: %w", err)
	}
	return cfg, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// httpClient 复用单一连接池；超时交给 ctx 而非 Client.Timeout，
// 以便对“整个调用链”做统一截止控制。
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// Metadata 是管道产出的发布元数据，连同 output.mp4 一起交给上层
// 调用方消费。DramaMate 本身不负责上传。
type Metadata struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	OutputPath  string   `json:"output_path"`
}

// ---------------------------------------------------------------------------
// 核心入口
// ---------------------------------------------------------------------------

// ProcessVideo 是整个本地加工管道的唯一入口：分离音频 -> 转录 -> 翻译 ->
// 烧录竖屏去重 -> 生成元数据。产出 output.mp4 与 metadata.json 两个工件，
// 上传/发布不在本项目范围，由上层调用方处理。
//
// 参数：
//   - ctx       : 控制超时 / 取消，贯穿所有子步骤。
//   - inputPath : 源 MP4 绝对/相对路径。
//
// 返回成片路径；任一步骤失败都会被包裹后向上返回。
func ProcessVideo(ctx context.Context, inputPath string) (string, error) {
	cfg, err := loadConfig()
	if err != nil {
		return "", fmt.Errorf("加载配置: %w", err)
	}

	if _, err := os.Stat(inputPath); err != nil {
		return "", fmt.Errorf("输入视频不可读 (%s): %w", inputPath, err)
	}

	// 预检：在做任何耗时工作前，先确认 ffmpeg 具备字幕烧录能力（libass）。
	// 否则前 3 步白跑，到步骤 4 才崩，且 ffmpeg 报错晦涩难懂。
	if err := preflightFFmpeg(ctx, cfg); err != nil {
		return "", fmt.Errorf("ffmpeg 预检: %w", err)
	}

	log := slog.With("input", inputPath)
	log.Info("管道启动")

	// —— 步骤 1：音频分离 ————————————————————————————————
	audioPath := filepath.Join(cfg.WorkDir, "audio.mp3")
	if err := extractAudio(ctx, cfg, inputPath, audioPath); err != nil {
		return "", fmt.Errorf("步骤1[音频分离]: %w", err)
	}
	log.Info("音频分离完成", "audio", audioPath)

	// —— 步骤 2：语音转录（中文 SRT）————————————————————
	zhSRT, err := transcribeAudio(ctx, cfg, audioPath)
	if err != nil {
		return "", fmt.Errorf("步骤2[语音转录]: %w", err)
	}
	log.Info("转录完成", "zh_srt_len", len(zhSRT))

	// —— 步骤 3：大模型翻译（英文 SRT）——————————————————
	enSRT, err := translateSRT(ctx, cfg, zhSRT)
	if err != nil {
		return "", fmt.Errorf("步骤3[字幕翻译]: %w", err)
	}
	enSRTPath := filepath.Join(cfg.WorkDir, "en.srt")
	if err := os.WriteFile(enSRTPath, []byte(enSRT), 0o644); err != nil {
		return "", fmt.Errorf("步骤3[写入英文SRT]: %w", err)
	}
	log.Info("翻译完成", "en_srt", enSRTPath)

	// —— 步骤 4：硬烧录 + 竖屏 + 去重 ————————————————————
	outputPath := filepath.Join(cfg.WorkDir, "output.mp4")
	if err := burnSubtitleAndDedup(ctx, cfg, inputPath, enSRTPath, outputPath); err != nil {
		return "", fmt.Errorf("步骤4[烧录去重]: %w", err)
	}
	log.Info("成片生成完成", "output", outputPath)

	// —— 步骤 5：LLM 生成发布元数据并落盘（供上层调用方消费）——
	meta, err := generateMetadata(ctx, cfg, enSRT)
	if err != nil {
		return "", fmt.Errorf("步骤5[生成元数据]: %w", err)
	}
	meta.OutputPath = outputPath
	metaPath := filepath.Join(cfg.WorkDir, "metadata.json")
	if err := writeMetadata(metaPath, meta); err != nil {
		return "", fmt.Errorf("步骤5[写入元数据]: %w", err)
	}
	log.Info("元数据已生成", "title", meta.Title, "tags", strings.Join(meta.Tags, ","), "metadata", metaPath)

	log.Info("管道完成 ✅ 产出 output.mp4 + metadata.json，待上层上传")
	return outputPath, nil
}

// writeMetadata 把元数据序列化为 metadata.json，供下游上传产品读取。
func writeMetadata(path string, meta Metadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化元数据: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入 %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 步骤 1：音频分离（os/exec 异步调用 ffmpeg）
// ---------------------------------------------------------------------------

// extractAudio 从 MP4 抽取音轨为 MP3。
// 使用 CommandContext 让 ctx 取消时能 kill 子进程，避免僵尸 ffmpeg。
func extractAudio(ctx context.Context, cfg *Config, inputPath, audioPath string) error {
	// -vn 丢弃视频流；-ar 16000 降采样到 16kHz，契合 Whisper 输入且减小体积。
	args := []string{
		"-y",
		"-i", inputPath,
		"-vn",
		"-ar", "16000",
		"-ac", "1",
		"-b:a", "128k",
		audioPath,
	}
	return runFFmpeg(ctx, cfg, "extract-audio", args)
}

// preflightFFmpeg 在管道启动前校验 ffmpeg 可用且支持字幕烧录（libass）。
// 缺 libass 时给出可操作的修复指引，避免前几步白跑、到烧录才崩。
func preflightFFmpeg(ctx context.Context, cfg *Config) error {
	cmd := exec.CommandContext(ctx, cfg.FFmpegBin, "-hide_banner", "-filters")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("ffmpeg 不可执行 (%s)，请确认已安装并在 PATH 中: %w", cfg.FFmpegBin, err)
	}
	// 字幕硬烧录依赖 subtitles/ass 滤镜，二者均由 libass 提供。
	if !bytes.Contains(out, []byte(" subtitles ")) && !bytes.Contains(out, []byte(" ass ")) {
		return errors.New("当前 ffmpeg 未编译 libass，无法硬烧录字幕。" +
			"修复: macOS 执行 `brew reinstall ffmpeg`（标准 bottle 自带 libass），" +
			"或自行编译时加 `--enable-libass --enable-libfreetype --enable-libfontconfig`")
	}
	return nil
}

// runFFmpeg 是对 ffmpeg 调用的统一封装：异步启动、捕获 stderr、受 ctx 控制。
func runFFmpeg(ctx context.Context, cfg *Config, tag string, args []string) error {
	cmd := exec.CommandContext(ctx, cfg.FFmpegBin, args...)

	// ffmpeg 把进度/错误都写到 stderr，仅在失败时回显，保持日志干净。
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	slog.Debug("执行 ffmpeg", "tag", tag, "args", strings.Join(args, " "))

	// 异步启动后再 Wait —— 体现“非阻塞拉起 + 显式等待”的调度模型，
	// 也方便未来在此处接入超时心跳 / 进度解析。
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg 启动失败 (%s): %w", tag, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		// ctx 取消时 CommandContext 会自动 kill，这里把语义透传出去。
		return fmt.Errorf("ffmpeg 被取消 (%s): %w", tag, ctx.Err())
	case err := <-done:
		if err != nil {
			return fmt.Errorf("ffmpeg 执行失败 (%s): %w\n--- stderr ---\n%s", tag, err, tail(stderr.String(), 1500))
		}
	}
	return nil
}

// tail 截取字符串末尾 n 个字符，避免错误日志过长。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// ---------------------------------------------------------------------------
// 步骤 2：语音转录（OpenAI 兼容 Whisper，multipart 上传）
// ---------------------------------------------------------------------------

// transcribeAudio 调用 OpenAI 兼容的 /audio/transcriptions 接口。
// MVP：若未配置 APIKey 则直接返回 Mock 中文 SRT；HTTP 框架已搭好可随时启用。
func transcribeAudio(ctx context.Context, cfg *Config, audioPath string) (string, error) {
	if cfg.APIKey == "" {
		return mockChineseSRT(), nil
	}

	f, err := os.Open(audioPath)
	if err != nil {
		return "", fmt.Errorf("打开音频: %w", err)
	}
	defer f.Close()

	// 构造 multipart/form-data：file + model + response_format=srt。
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filepath.Base(audioPath))
	if err != nil {
		return "", fmt.Errorf("创建表单文件域: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("写入音频数据: %w", err)
	}
	_ = mw.WriteField("model", cfg.WhisperModel)
	_ = mw.WriteField("response_format", "srt") // 直接拿 SRT，省去 JSON->SRT 转换
	_ = mw.WriteField("language", "zh")
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("关闭 multipart writer: %w", err)
	}

	url := cfg.APIBaseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", fmt.Errorf("构造转录请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	respBody, err := doJSONRequest(req)
	if err != nil {
		return "", fmt.Errorf("调用 Whisper: %w", err)
	}
	// response_format=srt 时返回纯文本 SRT，而非 JSON。
	return string(respBody), nil
}

// mockChineseSRT 返回一段标准中文 SRT，作为离线开发数据。
func mockChineseSRT() string {
	return strings.TrimSpace(`
1
00:00:00,000 --> 00:00:02,500
你以为把我赶出家门，我就会认输吗？

2
00:00:02,600 --> 00:00:05,000
三年后，整座城市都得听我的。

3
00:00:05,200 --> 00:00:08,000
当年看不起我的人，现在都跪着求我。
`) + "\n"
}

// ---------------------------------------------------------------------------
// 步骤 3：大模型翻译（NewAPI /chat/completions）
// ---------------------------------------------------------------------------

// 以下为 OpenAI 兼容 Chat Completions 的最小请求/响应结构体。
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// translateSRT 将中文 SRT 翻译为“北美网文爽点口语”英文 SRT。
// 关键约束（写进 System Prompt）：保留序号与时间轴，只替换字幕正文。
func translateSRT(ctx context.Context, cfg *Config, srtContent string) (string, error) {
	const systemPrompt = `You are an elite localizer for Chinese short-drama (cdrama) targeting a North-American audience on TikTok/YouTube Shorts.
Translate the given SRT subtitles from Chinese to English.

HARD RULES:
1. Keep the SRT structure EXACTLY: same indices, same timestamps, same blank-line separation. Only translate the dialogue text lines.
2. Do NOT translate literally. Rewrite into punchy, addictive, web-novel-style colloquial English that hits the "satisfaction/revenge/face-slap" beats fans crave.
3. Keep each subtitle short enough to read on a phone in the given time window.
4. Output ONLY the resulting SRT. No commentary, no code fences.`

	if cfg.APIKey == "" {
		return mockEnglishSRT(), nil
	}

	out, err := chatComplete(ctx, cfg, 0.8, []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: srtContent},
	})
	if err != nil {
		return "", fmt.Errorf("调用翻译模型: %w", err)
	}
	return out + "\n", nil
}

// chatComplete 是对 OpenAI 兼容 /chat/completions 的统一封装：
// 序列化 -> 发请求 -> 解析 -> 返回首个 choice 的文本内容。
// translateSRT 与 generateMetadata 共用，避免重复管线。
func chatComplete(ctx context.Context, cfg *Config, temperature float64, messages []chatMessage) (string, error) {
	payload, err := json.Marshal(chatRequest{
		Model:       cfg.LLMModel,
		Temperature: temperature,
		Messages:    messages,
	})
	if err != nil {
		return "", fmt.Errorf("序列化请求: %w", err)
	}

	url := cfg.APIBaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("构造请求: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	raw, err := doJSONRequest(req)
	if err != nil {
		return "", err
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("解析响应: %w", err)
	}
	if parsed.Error != nil {
		return "", fmt.Errorf("接口返回错误: %s (%s)", parsed.Error.Message, parsed.Error.Type)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("接口返回空 choices")
	}
	out := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if out == "" {
		return "", errors.New("返回内容为空")
	}
	return out, nil
}

// mockEnglishSRT 与 mockChineseSRT 时间轴一一对应。
func mockEnglishSRT() string {
	return strings.TrimSpace(`
1
00:00:00,000 --> 00:00:02,500
You kicked me out and thought I'd just give up?

2
00:00:02,600 --> 00:00:05,000
Three years from now, this whole city answers to me.

3
00:00:05,200 --> 00:00:08,000
Everyone who looked down on me? They're on their knees now.
`) + "\n"
}

// ---------------------------------------------------------------------------
// 步骤 4.5：LLM 自动生成发布元数据（标题 + 标签）
// ---------------------------------------------------------------------------

// generateMetadata 让 LLM 依据英文字幕内容，产出适配北美短视频平台的
// 爆款标题、简介与话题标签，替代写死的 Metadata。
//
// 约束：要求模型只输出严格 JSON，便于稳定解析；无 Key 时退化为 Mock。
func generateMetadata(ctx context.Context, cfg *Config, enSRT string) (Metadata, error) {
	if cfg.APIKey == "" {
		return mockMeta(), nil
	}

	const systemPrompt = `You are a viral short-video growth strategist for TikTok / YouTube Shorts targeting a North-American audience.
Given the English subtitles of a short video, craft metadata that maximizes click-through and watch-time.

Return STRICT JSON only (no markdown, no code fences), with EXACTLY this shape:
{
  "title": "scroll-stopping title, <= 80 chars, may use 1-2 emojis",
  "description": "1-2 punchy sentences hook + a call to action",
  "tags": ["5-8 lowercase hashtag keywords WITHOUT the # sign, niche + broad mix like fyp/viral"]
}
Base the content on what the video is ACTUALLY about. Do not invent a plot that isn't in the subtitles.`

	// 字幕可能很长，截断喂给模型即可（标题只需大意）。
	transcript := enSRT
	if len(transcript) > 4000 {
		transcript = transcript[:4000]
	}

	out, err := chatComplete(ctx, cfg, 0.9, []chatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: transcript},
	})
	if err != nil {
		return Metadata{}, fmt.Errorf("调用元数据模型: %w", err)
	}

	meta, err := parseMetaJSON(out)
	if err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

// parseMetaJSON 容错解析 LLM 返回的 JSON（剥离可能的 ```json 代码围栏）。
func parseMetaJSON(s string) (Metadata, error) {
	s = strings.TrimSpace(s)
	// 兜底：部分模型仍会包代码围栏，截取首个 '{' 到末个 '}'。
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	var raw struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return Metadata{}, fmt.Errorf("解析元数据 JSON 失败: %w (原文: %s)", err, tail(s, 300))
	}
	if raw.Title == "" {
		return Metadata{}, errors.New("元数据缺少 title")
	}
	// 规范化标签：去掉可能带的 # 前缀。
	for i, t := range raw.Tags {
		raw.Tags[i] = strings.TrimPrefix(strings.TrimSpace(t), "#")
	}
	return Metadata{Title: raw.Title, Description: raw.Description, Tags: raw.Tags}, nil
}

// mockMeta 提供离线占位元数据。
func mockMeta() Metadata {
	return Metadata{
		Title:       "She Was Abandoned… Now She Owns the City 🔥",
		Description: "Three years later, everyone who looked down on her is begging. Watch till the end 👀",
		Tags:        []string{"cdrama", "shortdrama", "revenge", "fyp", "viral"},
	}
}

// doJSONRequest 执行 HTTP 请求并对非 2xx 状态做统一错误处理。
func doJSONRequest(req *http.Request) ([]byte, error) {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 上限 16MB，防御超大响应
	if err != nil {
		return nil, fmt.Errorf("读取响应体: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("非预期状态码 %d: %s", resp.StatusCode, tail(string(data), 800))
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 步骤 4：FFmpeg 硬烧录 + 竖屏 1080x1920 + 算法去重
// ---------------------------------------------------------------------------

// burnSubtitleAndDedup 把英文 SRT 硬烧录进画面并做调色去重，画布由 VideoMode 决定：
//   - blur     ：竖屏 1080x1920，模糊放大画面作背景填充（默认，最像平台原生竖屏）
//   - crop     ：竖屏 1080x1920，放大裁切铺满，全屏清晰但裁掉左右边缘
//   - pad      ：竖屏 1080x1920，等比缩放 + PadColor 边补齐（默认黑边）
//   - colorpad ：pad 的别名，语义上强调用自定义边色（白边/品牌色）
//   - caption  ：竖屏 1080x1920，画面靠上、底部留 PadColor 字幕条，字幕不压画面
//   - original ：保持原视频分辨率/比例，仅去重 + 烧字幕
//
// blur 可调：BlurSigma(模糊强度)、FgScale(前景占宽比，<1 则前景缩小、模糊背景露更多)。
//
// 关键：SRT 不带画布分辨率(PlayRes)，libass 默认用极小画布排版后再整体放大，
// 会导致字号/边距被放大数倍、字幕糊满屏幕。竖屏模式必须用 original_size 告知真实
// 渲染分辨率(1080x1920)，字号与 MarginV 才会按实际画布生效。
func burnSubtitleAndDedup(ctx context.Context, cfg *Config, inputPath, srtPath, outputPath string) error {
	// subtitles 滤镜对路径里的特殊字符敏感，需转义冒号与反斜杠。
	escapedSRT := escapeForFilter(srtPath)

	// original 模式保持原尺寸，字幕画布随原视频；竖屏模式固定 1080x1920。
	subOrigSize := "1080x1920"
	if cfg.VideoMode == "original" {
		subOrigSize = ""
	}
	// caption 模式字幕落在底部字幕条，需要更大下边距把字推进色条区。
	marginV := 60
	if cfg.VideoMode == "caption" {
		marginV = 200
	}
	subtitles := buildSubtitlesFilter(escapedSRT, subOrigSize, marginV)

	const eq = "eq=brightness=0.01:contrast=1.02"
	var filterComplex string
	switch cfg.VideoMode {
	case "original":
		filterComplex = "[0:v]" + eq + "," + subtitles + "[v]"

	case "crop":
		// 放大裁切铺满：等比放大到至少覆盖 1080x1920，再居中裁切。
		filterComplex = "[0:v]scale=1080:1920:force_original_aspect_ratio=increase," +
			"crop=1080:1920," + eq + "," + subtitles + "[v]"

	case "pad", "colorpad":
		// 等比缩放 + 自定义边色补齐（居中）。
		filterComplex = fmt.Sprintf(
			"[0:v]scale=1080:1920:force_original_aspect_ratio=decrease,"+
				"pad=1080:1920:(ow-iw)/2:(oh-ih)/2:%s,%s,%s[v]",
			cfg.PadColor, eq, subtitles)

	case "caption":
		// 画面靠上（y=120），底部留色条放大字幕。
		filterComplex = fmt.Sprintf(
			"[0:v]scale=1080:1920:force_original_aspect_ratio=decrease,"+
				"pad=1080:1920:(ow-iw)/2:120:%s,%s,%s[v]",
			cfg.PadColor, eq, subtitles)

	default: // blur：竖屏模糊背景填充
		fgW, fgH := 1080, 1920
		if frac, err := strconv.ParseFloat(cfg.FgScale, 64); err == nil && frac > 0 && frac <= 1.0 {
			fgW = int(1080 * frac)
			fgH = int(1920 * frac)
		}
		filterComplex = fmt.Sprintf(
			"[0:v]split=2[bg][fg];"+
				"[bg]scale=1080:1920:force_original_aspect_ratio=increase,crop=1080:1920,gblur=sigma=%s[bgb];"+
				"[fg]scale=%d:%d:force_original_aspect_ratio=decrease[fgs];"+
				"[bgb][fgs]overlay=(W-w)/2:(H-h)/2,%s,%s[v]",
			cfg.BlurSigma, fgW, fgH, eq, subtitles)
	}

	args := []string{
		"-y",
		"-i", inputPath,
		"-filter_complex", filterComplex,
		"-map", "[v]",
		"-map", "0:a?", // 保留原音轨（若有）
		"-r", "30", // 统一帧率，进一步打散指纹
		"-c:v", "libx264",
		"-preset", "medium",
		"-crf", "23",
		"-c:a", "aac",
		"-b:a", "128k",
		"-movflags", "+faststart", // 利于流式播放/平台快速首帧
		outputPath,
	}
	return runFFmpeg(ctx, cfg, "burn-dedup-"+cfg.VideoMode, args)
}

// buildSubtitlesFilter 构造 subtitles 滤镜串。origSize 非空时传 original_size，
// 用于竖屏模式下让 libass 按真实渲染分辨率排版（避免字号被错误放大）。
func buildSubtitlesFilter(escapedSRT, origSize string, marginV int) string {
	style := fmt.Sprintf("force_style='FontName=Arial,FontSize=14,Bold=1,PrimaryColour=&H00FFFFFF,OutlineColour=&H80000000,BorderStyle=1,Outline=2,Shadow=1,Alignment=2,MarginV=%d'", marginV)
	if origSize != "" {
		return fmt.Sprintf("subtitles='%s':original_size=%s:%s", escapedSRT, origSize, style)
	}
	return fmt.Sprintf("subtitles='%s':%s", escapedSRT, style)
}

// escapeForFilter 转义 ffmpeg 滤镜图里路径的特殊字符（主要是 Windows 盘符冒号）。
func escapeForFilter(p string) string {
	p = strings.ReplaceAll(p, `\`, `\\`)
	p = strings.ReplaceAll(p, `:`, `\:`)
	p = strings.ReplaceAll(p, `'`, `\'`)
	return p
}

// ---------------------------------------------------------------------------
// main：CLI 入口
// ---------------------------------------------------------------------------

// version 由发布时通过 -ldflags "-X main.version=..." 注入；本地构建为 dev。
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "DramaMate %s\n用法: %s <input.mp4>\n产出 output.mp4 + metadata.json 到 WorkDir；上传不在本工具范围。\n", version, filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if a := os.Args[1]; a == "version" || a == "-v" || a == "--version" {
		fmt.Printf("DramaMate %s\n", version)
		return
	}
	inputPath := os.Args[1]

	// 全链路 30 分钟超时上限，可被信号/上游取消。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	outputPath, err := ProcessVideo(ctx, inputPath)
	if err != nil {
		slog.Error("管道失败", "err", err)
		os.Exit(1)
	}
	fmt.Println(outputPath)
}

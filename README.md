# DramaMate

> 短剧出海本地加工管道 —— 音视频处理 + AI 转录翻译 + 竖屏成片与发布元数据

`DramaMate` 把一条中文短剧 MP4，自动加工成带英文硬字幕的竖屏成片，并生成发布用的标题/标签元数据。整条管道用 Go 编写，视频处理直接调用本地 `ffmpeg`，转录/翻译走 OpenAI 兼容接口（NewAPI 转发）。

**职责边界**：DramaMate 只负责「加工」，产出 `output.mp4` + `metadata.json` 两个工件；**上传/发布不在本项目范围**，由上层调用方处理（不涉及 OAuth/平台 API）。

```
输入 MP4
  └─[1] ffmpeg 分离音频            → audio.mp3
        └─[2] Whisper 转录(中文)    → 中文 SRT
              └─[3] LLM 翻译(英文)   → en.srt
                    └─[4] ffmpeg 硬烧录 + 竖屏 + 去重 → output.mp4
                          └─[5] LLM 生成标题/标签        → metadata.json
                                                          ↓
                                              （交给上层调用方上传发布）
```

## 特性

- **全程 Context 控制**：超时 / 取消贯穿所有子步骤，ctx 取消即 kill 子进程，无僵尸 ffmpeg。
- **离线 Mock 模式**：未配置 API Key 时，转录/翻译自动降级为 Mock 数据，方便本地跑通闭环。
- **6 种竖屏画布模式**：`blur`/`crop`/`pad`/`colorpad`/`caption`/`original`，环境变量切换（见下）。
- **算法去重**：竖屏重排 + 轻微调色（`brightness=0.01` / `contrast=1.02`）+ 统一帧率，打散平台查重指纹。
- **本地化翻译**：System Prompt 引导 LLM 把台词改写成北美网文「爽点」口语，而非直译。
- **元数据自动生成**：LLM 依据字幕内容产出爆款标题、简介、话题标签，落盘 `metadata.json`。

## 环境要求

- Go ≥ 1.26
- 本地 `ffmpeg`（**必须编译带 `libass`**，否则字幕烧录不生效）
- 一个 OpenAI 兼容的 API 端点（NewAPI / 官方）—— 可选，缺省走 Mock

```bash
# macOS
brew install ffmpeg
# 验证 libass 支持（字幕烧录必需）
ffmpeg -filters | grep subtitles
```

> ⚠️ **字幕烧录需要 libass**。若 `ffmpeg -filters | grep subtitles` 无输出，说明你的 ffmpeg 未编译 libass，步骤 4 会失败。
> 解决：装 `ffmpeg-full`（含 libass），或下载一个带 libass 的静态 ffmpeg，再用 `DRAMA_FFMPEG_BIN` 指向它：
> ```bash
> DRAMA_FFMPEG_BIN=/path/to/ffmpeg ./dramamate input.mp4
> ```
> 管道启动时有 `preflightFFmpeg` 预检，缺 libass 会立即报错而非烧录时才崩。

## 快速开始

```bash
git clone https://github.com/DramaMate/DramaMate.git
cd DramaMate
go build -o dramamate .
```

### 离线 Mock 模式（不调真实 API，验证管道闭环）

```bash
./dramamate input.mp4
```

### 接入真实 API

```bash
export DRAMA_API_BASE_URL="https://api.openai.com/v1"   # 或你的 NewAPI 域名/v1
export DRAMA_API_KEY="sk-xxx"
export DRAMA_WHISPER_MODEL="whisper-1"
export DRAMA_LLM_MODEL="gpt-4o"
export DRAMA_VIDEO_MODE="blur"

./dramamate input.mp4
# 完成后在 WorkDir 得到 output.mp4 与 metadata.json
```

## 配置项（环境变量）

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `DRAMA_API_BASE_URL` | OpenAI 兼容端点 | `https://newapi.example.com/v1` |
| `DRAMA_API_KEY` | Bearer Token，**留空则走 Mock** | *(空)* |
| `DRAMA_WHISPER_MODEL` | 转录模型 | `whisper-1` |
| `DRAMA_LLM_MODEL` | 翻译/元数据模型 | `claude-sonnet-4-6` |
| `DRAMA_WORK_DIR` | 工件输出目录 | `./work` |
| `DRAMA_FFMPEG_BIN` | ffmpeg 可执行路径 | `ffmpeg`（依赖 PATH） |
| `DRAMA_VIDEO_MODE` | 画布模式（见下） | `blur` |
| `DRAMA_PAD_COLOR` | pad/colorpad/caption 边色 | `black` |
| `DRAMA_BLUR_SIGMA` | blur 模糊强度 | `20` |
| `DRAMA_FG_SCALE` | blur 前景占宽比(0.5~1.0) | `1.0` |

### 画布模式（`DRAMA_VIDEO_MODE`）

| 模式 | 效果 |
|------|------|
| `blur` | 竖屏 1080x1920，模糊放大画面作背景填充（默认，最像原生竖屏） |
| `crop` | 竖屏 1080x1920，放大裁切铺满，全屏清晰但裁掉左右边缘 |
| `pad` | 竖屏 1080x1920，等比缩放 + 黑边 |
| `colorpad` | 同 pad，但边色由 `DRAMA_PAD_COLOR` 控制（白边/品牌色） |
| `caption` | 画面靠上 + 底部色条放大字幕（解说/meme 风） |
| `original` | 保持原视频分辨率/比例，仅去重 + 烧字幕 |

## 命令行用法

```
dramamate <input.mp4>
```

成功后在 `WorkDir` 产出：

```
output.mp4       # 加工后的成片
metadata.json    # 标题/简介/标签/成片路径，供上层调用方上传消费
audio.mp3 en.srt # 中间产物
```

`metadata.json` 结构：

```json
{
  "title": "爆款标题",
  "description": "简介 + CTA",
  "tags": ["tag1", "tag2", "fyp", "viral"],
  "output_path": "/abs/path/to/output.mp4"
}
```

## 管道步骤一览

| 步骤 | 函数 | 状态 |
|------|------|------|
| 1 音频分离 | `extractAudio` → `runFFmpeg` | ✅ 真实实现 |
| 2 语音转录 | `transcribeAudio` | ✅ 真实实现 / 无 Key 降级 Mock |
| 3 大模型翻译 | `translateSRT` | ✅ 真实实现 / 无 Key 降级 Mock |
| 4 烧录去重 | `burnSubtitleAndDedup` | ✅ 真实实现（6 画布模式） |
| 5 元数据生成 | `generateMetadata` → `writeMetadata` | ✅ 真实实现 / 无 Key 降级 Mock |

> 上传/发布不在本项目范围 —— 由上层调用方读取 `output.mp4` + `metadata.json` 完成。

## 避坑提示

1. **ffmpeg 必须带 libass**：否则 `subtitles` 滤镜不可用、步骤 4 失败。预检会提前报错。
2. **SRT 字幕缩放**：SRT 不带画布分辨率，libass 默认用极小画布排版后整体放大，会导致字号/边距被放大数倍、字幕糊满屏。竖屏模式已用 `original_size=1080x1920` 修正。
3. **ffmpeg 字幕滤镜路径转义**：Windows 盘符冒号、单引号会让滤镜图解析失败，已由 `escapeForFilter` 处理。
4. **去重力度**：当前调色为极轻扰动，平台查重升级后可能不够——可叠加随机裁剪、镜像、变速、加噪，但注意别破坏观感。
5. **横屏源**：从抖音下载的视频不一定是竖屏；横屏源需用 `blur`/`crop`/`pad` 转竖屏，`original` 会保持横屏（非标准 Shorts 版位）。

## ⚠️ 合规声明

本项目用于**自有版权内容**的多平台分发自动化。使用者须确保对处理的短剧内容拥有合法授权，并遵守各目标平台的服务条款与所在地法律法规。「算法去重」仅用于规避同一账号多平台重复内容的限流，不得用于侵犯他人版权或规避内容审核。

## 路线图

- [ ] 源片硬字幕去除（OCR 定位 + 遮盖/裁切）
- [ ] caption 模式字幕位置微调（沉入色条中央）
- [ ] 批量队列 + Goroutine 并发调度
- [ ] 工件交接规范（metadata schema 版本化）

## 项目结构

```
DramaMate/
├── go.mod
├── main.go      # 本地加工管道（5 步，无上传）
├── bin/ffmpeg   # 自带的带 libass 静态 ffmpeg（gitignore，可选）
└── README.md
```

## License

MIT

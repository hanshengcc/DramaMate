# Changelog

本项目遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

## [0.2.0] - 2026-06-01

### Added
- **`preview` 子命令**：`dramamate preview <input.mp4> [ts]` 在指定时间点抽一帧，
  对全部 6 种画布模式（blur/crop/pad/colorpad/caption/original）各渲染一张缩略图到
  `WorkDir/preview/preview_<mode>.png`。**跳过转录与翻译、亚秒级、零 API 消耗**，
  用于在投入完整加工前可视化挑选竖屏构图。
- `version` 子命令（`dramamate version`），版本号由发布构建注入。

### Changed
- 将画布几何抽取为单一来源 `canvasBodyVF`，由烧录与预览共用——保证两者输出一致、
  消除重复滤镜逻辑（内部重构，无行为变化）。

## [0.1.0] - 2026-05-31

### Added
- 首个发布。本地短剧本地化加工管道：ffmpeg 分离音频 → Whisper 转录 →
  LLM 翻译 → 英文字幕硬烧录 + 竖屏构图 + 去重 → 生成 `metadata.json`。
- 6 种竖屏画布模式，环境变量可配（`DRAMA_VIDEO_MODE` 等）。
- 无 API Key 时转录/翻译降级为 Mock，便于离线跑通。
- 跨平台 GitHub Release（GoReleaser）：linux/darwin/windows × amd64/arm64。

[Unreleased]: https://github.com/hanshengcc/DramaMate/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/hanshengcc/DramaMate/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/hanshengcc/DramaMate/releases/tag/v0.1.0

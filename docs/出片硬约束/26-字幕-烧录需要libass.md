# 烧字幕依赖 libass

**规则**：`subtitles` / `ass` 滤镜需要 ffmpeg 带 libass 编译。

**验证**：`ffmpeg -filters | grep subtitles`，有输出才可用。没有的话烧字幕会静默失败或报 "Filter not found"。

**注意**：本项目 ffmpeg 是预编译分发的，验一次就够；换 ffmpeg 版本后要重验。

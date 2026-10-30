# 终端渲染审阅：Bug 最小复现

英文版报告（面向上游）：[review/BUGS.md](../BUGS.md)。本文件是中文工作索引。

约定：**每条测试断言"正确行为"**。所以它们现在是红的 —— 13 个 FAIL 就是这份
交付物，失败信息里写清了现象、期望值与实际值、以及修法。修掉一个 bug，对应测试
转绿；13 条全绿即验收完成。

## 怎么跑

```sh
# 全部自动复现（不需要终端，Windows 也能跑）
# 预期：13 个 FAIL，go test 退出码非 0
go test -tags repro ./review/repro ./ui -run 'TestB' -v

# 单个
go test -tags repro ./ui -run TestB07 -v
go test -tags repro ./review/repro -run TestB04 -v
```

自动复现都在 `-tags repro` 后面，默认的 `go build ./...` / `go test ./...` / lint
完全不涉及它们。

需要真终端的只剩两条，都在 `manual/` 下（POSIX sh，WSL / Linux / macOS）：

```sh
sh review/repro/manual/B03_suspend.sh
sh review/repro/manual/B13_bracketed_paste.sh
```

两条脚本都**强制 `GLOW_TEXT_SIZING=on`**：只有走 raw-painter 路径，这两个 bug 才
存在。在 Windows brush 上无法运行（没有 `/dev/tty` 的 raw terminal，
`ui/rawpaint_windows.go` 恒返回错误，glow 永远走默认 renderer），必须用 WSL。

## 一览

"实测"列是 bug 存在时的数字，也就是每条测试失败信息里的 actual。

| ID | 问题 | 位置（代码注释） | 类型 | 复现 | 实测 |
|----|------|------------------|------|------|------|
| B01 | 回退到默认 renderer 时 text sizing 仍开启，标题文字整体消失 | `ui/ui.go` NewProgram 的 `BUG: this fallback keeps text sizing enabled` | 正确性 | `TestB01_HeadingTextSurvivesTheFallbackRenderer` | 渲染器输出只剩 `AB` |
| B02 | `stop()` 之后仍在执行的 paint 命令把整帧画回主屏 | `ui/rawpaint.go` `BUG: no p.done check` | 正确性 | `TestB02_NoPaintAfterStop` | 写回 33 字节整帧 |
| B03 | ctrl+z 把终端留在 raw + alt screen + 隐藏光标 | `ui/ui.go` ctrl+z 的 `BUG: suspend is broken` | 正确性 | `manual/B03_suspend.sh` | `stty -a` 仍是 `-icanon -echo`（WSL 已复现） |
| B04 | 缩放后的标题超出终端宽度，行数补偿失效 | `utils/textsize.go` `ApplyTextSizing` 的 `BUG: a scaled heading can end up wider` | 正确性 | `TestB04_ScaledHeadingFitsTheTerminal` | 40 列终端上 81 列，漂移 2 行 |
| B05 | raw painter 不裁剪、不补齐 frame | `ui/rawpaint.go` `WARNING: the frame is written verbatim` | 正确性 | `TestB05_FrameIsClampedToTheTerminal` | 24 行终端写 30 行 |
| B06 | fsnotify watcher 创建失败后 nil 解引用 | `ui/pager.go` `BUG: m.watcher stays nil` | 正确性 | `TestB06_WatchFileWithNilWatcherDoesNotPanic` | nil pointer panic |
| B07 | 首帧在 viewport 宽度 0 时渲染（不换行） | `ui/pager.go` `BUG: on the first render the viewport width is still 0` | 正确性 | `TestB07_FirstRenderWrapsAtTheConfiguredWidth` | wrap=40 却产出 151 列 |
| B08 | 开行号时 `MaxWidth` 把正文截掉几列 | `ui/pager.go` `WARNING: MaxWidth truncates, it does not re-wrap` | 正确性 | `TestB08_LineNumbersKeepAllText` | 36 个字符只剩 34 |
| B09 | 文档状态下 `errMsg` 被丢弃 | `ui/ui.go` `BUG: errMsg is never handled here` | 正确性 | `TestB09_ReloadErrorIsShown` | 仍显示旧内容，无任何报错 |
| B10 | 两位数 heading level 的 marker 原样漏到终端 | `utils/textsize.go` `markerLevel` 的 NOTE | 正确性（潜伏） | `TestB10_NoRawMarkerReachesTheTerminal` | 输出 `\x1b]6666;10\x1b\` |
| B11 | 每条消息都整屏重绘，无合并/无帧预算 | `ui/rawpaint.go` `NOTE: the generation counter ...` | 性能 | `TestB11_UnchangedFrameIsNotRepainted` | 50 条相同消息 → 50 次 `\x1b[2J` |
| B12 | 每条消息渲染两遍 View | `ui/rawpaint.go` `PERF: inner.View() is rendered here` | 性能 | `TestB12_OneViewRenderPerMessage` | 2 次/消息 |
| B13 | raw painter 没镜像 bracketed paste，多行粘贴变成按键 | `ui/rawpaint.go` `start()` 的 `BUG: ...bracketed paste gap...` | 正确性 | `TestB13_MultiLinePasteArrivesAsKeyPresses` + `manual/B13_*` | 换行解码成 `ctrl+j` |
| B15 | `watchResize` 在 `Run` 未被调用时 goroutine + SIGWINCH 泄漏 | `ui/rawpaint.go` `NOTE: prog.Send() blocks until the event loop is running` | 正确性 | `TestB15_WatchResizeStopsAfterStop` | `stop()` 后仍在 `Send` 里阻塞 |

## 逐条说明

### B01 回退路径吃掉标题（自动）

`glow` 判定终端支持 OSC 66 后，`ui.NewProgram` 尝试 `openRawTerminal()`；失败时
退回 `tea.NewProgram(m)`，但 `utils.TextSizingEnabled()` 的结论是 `sync.OnceValue`
缓存的，无法关掉。默认 renderer 会把每帧解析进 cell buffer，OSC 66 连同 payload
一起被丢弃 —— 丢的是标题正文本身，不是转义序列。

```sh
go test -tags repro ./review/repro -run B01 -v
```

复现用 `charmbracelet/ultraviolet`（bubbletea 默认 renderer 就是用它）解析同一帧，
输出 `screen after the cell renderer: "... Body text. ..."`：正文在，`Hello` 不在。
触发条件：`GLOW_TEXT_SIZING=on` 且 `/dev/tty` 不可用（Windows 恒成立）。

### B02 退出后补画一帧（自动）

`q` 触发的 `Update` 仍然 batch 了一个 paint 命令；bubbletea 关闭时不等待命令
（`handleCommands` 直接放弃 goroutine），`cleanup` 里的 `painter.stop()` 于是可能先
执行完，随后那一帧被写到已恢复的主屏上。测试断言 `stop()` 之后一个字节都不写。

### B03 ctrl+z（手工，WSL 已确认）

bubbletea 的 `p.suspend()` 只通过 renderer 释放终端，`nilRenderer.reset()` 是空操作；
模型要等到 SIGCONT 之后才收到 `SuspendMsg`。所以挂起期间 shell 拿到的是 raw mode、
alt screen、隐藏光标。脚本会打印逐步检查和恢复方法。

### B04 缩放标题溢出（自动）

glamour 在 `bs.Width(ctx)`（正文换行宽度）处把标题文本换行，随后 `s=3` 把推进宽度
也乘 3；之后没有任何重新换行或裁剪，而仓库里所有宽度工具都算不准这个序列
（reflow 把整段转义当可打印字符，lipgloss 把它当零宽）。复现用
`review/repro/width.go` 的 `DisplayWidth` 模拟"遵守规范的终端"，得出 40 列终端上
81 列、并且屏幕行数比 viewport 统计多 2 行。

### B05 frame 不受约束（自动）

`paint()` 不知道终端尺寸：不限行数、不裁列宽。原来吸收这种情况的是 renderer，现在
它被绕过了。所以任何一行被终端折行、或多出一行，都会把整帧上滚并把 pager 的原点
移走。B04、B07 就是产生这种帧的源头。

### B06 nil watcher panic（自动）

`initWatcher()` 失败只记日志，`m.watcher` 留空；`contentRenderedMsg` 之后
`watchFile` 照旧入队，`m.watcher.Add(dir)` 直接 nil 解引用。测试用零值
`pagerModel` 调 `watchFile`，断言不 panic（现在会 panic）。

### B07 首帧宽度为 0（自动）

`pagerModel.Init` 排队的 `renderWithGlamour` 在第一个 `WindowSizeMsg` 之前运行，
`m.viewport.Width()` 还是 0，于是 `min(GlamourMaxWidth, 0) = 0`，
`glamour.WithWordWrap(0)` 等于不换行。整篇文档被摊成一行（实测 151 列）画一次；
而且这次渲染和 resize 触发的渲染走同一个命令队列、没有顺序保证，结论可能"赢"到
下一次 resize 才被纠正。

### B08 行号截断正文（自动）

`trunc = MaxWidth(viewport.Width() - 4)` 是截断而不是重新换行，但 glamour 按
`width`（= viewport 宽度）换行；段落还有 2 列左边距，所以正好停在右边界的那一行会
被切掉 2 列（边距更宽的结构最多 4 列）。测试对比开/关行号两份渲染的字符数，断言
相等（现在是 34 vs 36）。

### B09 reload 错误被吞（自动）

文档状态下 `model.Update` 没有 `errMsg` 分支，消息只转发给当前子模型；pager 也没有
`errMsg` 分支（只有 stash 有）。复现是真的端到端：打开文件 → 删掉 → 按 `r`
（`loadLocalMarkdown` 返回 `errMsg`）→ 视图仍是旧内容、没有任何错误提示。

### B10 两位数 level 的 marker（自动，潜伏）

`markerOpen(level)` 能写多位数，`markerLevel` 只认一位；`10` 的 marker 于是不被识别，
`ApplyTextSizing` 把前缀原样写回，ESC 序列漏到终端。当前没有 level>3 的标记，所以是
潜伏问题。测试断言 `ApplyTextSizing` 消费掉自己全部 marker（`\x1b]6666;` 不得出现在
输出里）；level 4 和 9 现在是对的对照（正确退化成纯文本），level 10 失败。

### B11 每消息整屏重绘（自动，性能）

`paintModel.Update` 对每条消息都排一个 `\x1b[?2026h\x1b[H\x1b[2J ... ?2026l` 的整帧。
generation 计数只防乱序，不防频率；被替换的 renderer 有 60fps 合并与跳帧。测试连发
50 条内容相同的消息，断言只重绘一次（现在每条都 `\x1b[2J`）。

### B12 每消息两次 View（自动，性能）

`Update` 里为了 paint 算一次 `inner.View()`，bubbletea 随后又为 `nilRenderer`
算一次（`tea.go:872` Update、`tea.go:880` render → `View`）。第二次结果被丢弃。
计数模型统计到一条消息渲染 2 次，断言 1 次。

### B13 没有 bracketed paste（自动 + 手工）

raw painter 取代了 renderer，却只镜像了 alt screen、光标和鼠标；bracketed paste
（`?2004h`）没有开。于是终端把粘贴的整块内容当普通字节发过来，换行变成独立按键：
用 bubbletea 同一个 decoder 解码 `"doc\nq"` 得到 `d, o, c, ctrl+j, q`，而
`ui/stash.go` 的 `handleFiltering` 把 `ctrl+j` 当 Enter 处理 —— 过滤器当场提交，
第二行的 `q` 就成了文件列表里的按键，直接退出 glow。

自动测试先解码字节、再断言 `start()` 发出 `?2004h`（`stop()` 要发 `?2004l`），
所以不需要终端。手工脚本给的是肉眼版本。

> 上一版这个脚本**没能复现**，原因是它没有强制 `GLOW_TEXT_SIZING=on`：在
> alacritty/wezterm 这类不支持 OSC 66 的终端上，glow 走的是默认 renderer（bracketed
> paste 是开的），粘贴被当成 `PasteMsg` 交给 textinput，换行被 `ReplaceNewlines(" ")`
> 折叠成空格，于是过滤器里出现 `doc q` —— 看起来"正常"，但根本没进 raw 路径。
> 两条手工脚本现在都强制该环境变量。

### B15 watchResize 泄漏（自动）

`prog.Send` 在 `Run` 之前是阻塞的（`msgs` 无缓冲），所以 `stop()` 关掉 `done` 也唤不
醒它 —— goroutine 停在 `Send` 里，SIGWINCH handler 也不会注销。测试用
`runtime.Stack(all)` 查找 `rawPainter).watchResize`：`stop()` 之后仍然存在
（断言它应该消失）。

## 已降级：不可观测，不再作为 bug 计数

你实测后这三条撤下来了：它们要么需要特殊环境，要么根本没有用户可见后果。代码里
保留 NOTE，但不进 bug 表、不配脚本。

**B14（共用 `os.Stdout`，无共同锁）** —— 原本写"帧会被写坏"，夸大了。帧本身由
一次 `io.WriteString` 发出，`os.File.Write` 内部循环写完，别的写者最多把一段完整
序列插在帧的两次 syscall 之间：终端收到 `?2026h + 帧前半 + OSC + 帧后半 + ?2026l`，
仍然是合法流，OSC 会被正常处理。要真的破坏，必须让某一次 write 在序列中间返回，
这在 tty 上不会发生。属于"两个所有者共用一个 fd 但没有协调"的卫生问题。

**B16（帧被中断后 `?2026h` 未关闭）** —— alacritty 和 wezterm 都会在批次结束时
flush，所以你那边立刻返回；只有 kitty 这类严格缓冲到 `?2026l` 的终端才会"卡住"。
触发条件还是"进程在两次写之间死掉"，不可稳定复现。保留为一行防御性修复建议
（`stop()` 里无条件发 `?2026l`）。

**B17（probe 往 tty 写 CPR 查询）** —— 可测的部分已经测到：普通
`glow README.md` 的 pty 捕获里确实有 1 个 `ESC [ 6 n` 和 1 个 OSC 66，说明非 TUI
路径也会 probe。但"迟到的回复变成按键"需要终端应答慢于 800ms，无法稳定复现，也
没有用户可见症状。保留为 NOTE。

## 维护性（无脚本）

- `TextSizingEnabled` 的 `sync.OnceValue` 缓存无法回退（B01 的根因之一）。
- `AddHeadingSizeMarkers` 原地改写配置；`styleConfigFor` 只做了结构体浅拷贝，指针字段
  仍与 `styles.DefaultStyles` 共用。
- `#` 前缀被整段丢弃、非 `#` 前缀被保留并缩放，两种视觉结果出自同一条规则。
- `osc66TightChunks` 的 `s=2:n=3:d=4` 与 `w=(width*3+3)/4` 是未注明出处的魔数。
- `windowSize()` 的 80x24 会把"未知尺寸"伪装成"已知尺寸"（现有测试
  `TestPaintModelZeroSizeSubstitution` 已覆盖该行为）。
- 每次 resize 都整篇重新 glamour 渲染，启动时还有两次渲染互相竞争（`pager.go` 的
  `case tea.WindowSizeMsg` NOTE）。
- 缩进换行前的补偿换行落在 SGR reset 之前，空行继承标题样式（今天无害，因为带背景
  的标题不会被标记）。

## 备注

- `review/repro/repro_test.go` 直接 import `charmbracelet/ultraviolet`（bubbletea 用来
  建 cell buffer 的库），它目前在 `go.mod` 里是 `// indirect`；因为复现文件在
  `-tags repro` 后，默认构建/测试/lint 不涉及它。若执行 `go mod tidy`，它会被提升为
  直接依赖。
- 所有自动复现只读文件系统（`t.TempDir()`），不写工作区。

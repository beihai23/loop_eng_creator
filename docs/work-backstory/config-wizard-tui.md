---
arc: config-wizard-tui
started: c17f93e
status: active
commits: []
---

# 配置向导 TUI 化 + 标签前缀模型（config 体验重构）

## Intent
用户实测 `loop-eng config` 后两轮差评：① 提示文字、选项挤在一起无颜色无层次，
`task_label` 裸输入对小白毫无意义（"有特殊需求的话又怎么办呢???"——说明文本
制造了疑问却没给答案）；②「我要优秀的 TUI 体验，做一切必要的改造，包括测试」。
中途又挖出两个设计问题：自定义标签只输入名字够不够（不够——标签必须真实存在于
仓库，且还有全套 loop:* 状态标签）；自定义该是单标签、整组、还是前缀（判断：前缀——
整组逐个配是过度工程，单标签隔离不干净且让用户以为配完了其实只配了一半）。

## Process
- **双模式架构**：TTY → bubbletea 全屏向导（alt-screen 不污染滚动历史；lipgloss
  配色：标题/暗灰解释/高亮选中/警告/错误）；非 TTY → 行模式兜底（piped 脚本契约
  一字不破）。零新增依赖——bubbletea/lipgloss 本来就是 dashboard 的依赖，isatty
  本就随 termenv 在模块图里（tidy 后转直接依赖）。
- **向导模型写成纯状态机**：Update 只依赖 tea.KeyMsg 不碰终端，测试直接喂按键断言
  状态迁移+落盘 config——不需要 teatest 之类的 TTY 测试框架。
- **task_label 死胡同三连修**：裸输入 →「用默认/自定义」选择题 → 自定义页留空
  只报错且无返回键（死胡同）→ 留空=回退默认。最终形态：每个分叉默认项在首位、
  每个输入页留空都能安全退回默认——不需要 esc 返回键也不会困住用户。
- **一键创建标签**：旧流程配完 labels 还得手工建（preflight 会拦 daemon）。向导
  GitHub 流程加「现在就创建」步——gh label create --force 幂等整组（任务标签+
  状态族），与 preflight 校验共用同一集合（channel.RequiredGitHubLabels 导出为
  单一真相源）。ghLabelCreate 做成可注入包变量——没 stub 时测试对假 repo 真调 gh
  跑了 27 秒，这类事故从根上杜绝。
- **前缀模型（label_prefix）**：自定义前缀 ai: → ai:task + ai:running + ai:done…。
  工程要点：config 加字段（空=loop: 零迁移）；channel/github 的 UpdateStatus /
  EnsureLabels / statusLabelsToRemove 全部前缀化；preflight 同集合派生；daemon
  reconcile 不再硬编码 loop:blocked（channel.StatusLabeler 可选接口）。
- **审计抓到的两个真 bug**：① statusLabelsToRemove 硬编码 "loop:" 剥离会误删
  别实例的 loop:task 任务身份证（多实例共存的边缘 bug，前缀化顺带根治——互斥
  只剥自己前缀）；② 再配置抹除：已有 ai: 配置重跑向导选「默认」会把 label_prefix
  写回 ""（wizard 零值覆盖 cfg），task_label 与 label_prefix 撕裂——newWizard
  从 cfg 预填，「默认」= 保持现状。

## Decisions
- **前缀 > 单标签 > 整组**：整组逐个配否决（状态族是有互斥语义的协议，不是配置面）；
  单标签隔离不干净（别家前缀误删 + 观感只配了一半）；前缀一个字段解决多实例共存
  与命名规范两类真实需求。
- **task_label 与 label_prefix 独立**：身份标签可以是任何名字，状态族只看前缀；
  向导自定义前缀时派生 task_label=prefix+"task"（大多数用户的心智模型），exotic
  组合手编 yaml 仍可行。
- **默认即保持现状**：向导所有「默认/留空」路径都不覆盖现有配置——再配置场景
  （重跑向导）安全。
- **行模式不加前缀提示**：它是脚本/兼容通道，前缀走 TTY 向导或手编 yaml。

## Lessons
- TUI 测试的正解是把模型写成纯状态机喂消息，而不是找终端测试框架。
- 可注入的外部调用点（ghLabelCreate）不只是可测性——没有它，测试会对假目标
  发起真实网络调用（27s/个），且随网络环境 flaky。
- 「默认」在向导里必须始终等于「保持现状」，否则再配置场景必然撕裂配对字段。

## Related
- internal/cli/config_wizard.go（bubbletea 向导）+ config_wizard_test.go（状态机测试）
- internal/cli/config.go（TTY 分流 + 行模式文案）/ run_once.go（buildChannel 接线前缀）
- internal/config/config.go（Channel.LabelPrefix）
- internal/channel/github.go（前缀化 + StatusLabel）、channel.go（StatusLabeler 接口）、
  preflight.go（RequiredGitHubLabels(prefix, taskLabel)）
- internal/daemon/engine.go（reconcile blockLabel 从 channel 取）
- 测试：status_label_mutex*_test（含跨实例隔离）、github_labels_tier1_test（前缀端到端）

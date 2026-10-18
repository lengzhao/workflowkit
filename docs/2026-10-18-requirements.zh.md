# workflowkit 需求分析（v0.3 · 嵌入式 + 开放节点类型）

> **定位**：**嵌入式** Go 编排库。宿主 import 它，把流程写成声明式定义（JSON/YAML/Go struct）执行。
> **核心特征**：**节点类型是可插拔的** —— 内置 5 种，用户可以注册自己的。
> **不做什么**：不调度、不触发、不持久化、不联网、不依赖宿主框架。库只提供「注册 → 解析 → 执行」。
> 目标：**简洁、易用、可嵌入、可扩展**。

---

## 0. 一句话理解

```go
// 1) 节点类型用包级 init() 注册（照搬 AgentKit/pluginkit），import 即生效
import (
    _ "github.com/lengzhao/workflowkit/nodes"       // 内置 5 种
    _ "github.com/lengzhao/workflowkit/nodes/http"  // 自定义：init() 里 Register("http", NewHTTP)
)

// 2) 宿主能力在每次运行时传入
env := workflowkit.Env{Shell: sh, Tools: rt, LLM: llm, Agents: ag, Files: fs}

wf, err := workflowkit.Parse(data)                  // 解析 + 加载期校验
res, err := workflowkit.Run(ctx, wf, inputs, env)   // 执行
fmt.Println(res.Text)
```

**唯一的 API 面**：`Register` / `Parse` / `Run`（+ `ListTypes` / `Describe` 供诊断，照搬 pluginkit 的 `ListKinds` / `Describe`）。**没有 `New`/`Engine` 对象**：注册是全局的，能力是运行期传入的 —— 库里没有需要维护状态的实例。

---

## 1. 设计原则

| # | 原则 |
|---|---|
| P1 | **嵌入式优先**：被 import，不是被部署。无常驻进程、无全局状态、无后台 goroutine。 |
| P0 | **无状态 API**：没有 Engine 对象；注册是包级全局（init），能力在 `Run` 时传入。 |
| P2 | **内置即插件**：`script`/`tool`/`model`/`agent`/`foreach` 与用户自定义节点走**完全相同的注册与执行路径**。不搞两套。 |
| P3 | **一切皆扩展点**：节点类型开放。注册方式照搬 AgentKit/pluginkit（`init()` + kind + 构造函数）。 |
| P4 | **零宿主依赖**：核心只依赖标准库 + JSON Schema 校验。宿主能力走 interface，**运行期传入**。 |
| P5 | **加载期能查的一律加载期报**：字段、模板语法、依赖引用、成环、节点 config schema，都在 `Parse` 暴露。 |
| P6 | **主上下文不接触大 payload**：step 间 by reference，只有 `result` 回到调用方。 |
| P7 | **表达式极简**：`text/template` + 3 个函数，一屏讲完。 |
| P8 | **约定优于配置**：能默认的都默认（串行、层内并发 4、agent 超时 900s）。 |

---

## 2. 节点类型（扩展点）

### 2.1 注册方式：照搬 AgentKit / pluginkit 的形态

**包级全局注册 + `init()` + kind 字符串 + 构造函数 `(T, error)`**：

```go
// 每个节点类型所在的包，在 init() 里登记 kind 与构造函数：
func init() {
    workflowkit.Register("http", NewHTTP)
}

// 构造函数只有三种合法形态（与 pluginkit 一致，其余形态 panic）：
func NewHTTP(cfg HTTPConfig) (workflowkit.Node, error)
// 或
func NewHTTP() (workflowkit.Node, error)
```

| 规则 | 说明 |
|---|---|
| **kind 是扁平字符串** | `http`、`sql`、`saga`。**不做** `node/http` 这种命名空间分组（pluginkit 的注册表是 `map[string]Spec`，没有分组能力）；冲突靠约定前缀规避 |
| **重复注册 panic** | 与 pluginkit 一致，`init()` 阶段尽早失败 |
| **构造函数形态** | 只允许 `New() (T, error)` / `New(cfg) (T, error)`，其余在注册时 panic |
| **无 deps 参数** | 这是**与 pluginkit 的一处刻意差异**：宿主能力在运行时经 `env` 传入 `Run`，不是构造期注入 —— 因为这是嵌入式库，能力随每次调用传入，不是装配期固定的实例图。理由见 §2.3 |
| **注册是全局副作用** | 与 pluginkit 相同：`import _ ".../nodes/http"` 即完成注册；库用 `ListTypes()` / `Describe(kind)` 照搬 pluginkit 的 `ListKinds()` / `Describe()` |

### 2.2 两个接口，就是全部

```go
type Node interface {
    // Run 执行一步。env 携带宿主能力（每次调用传入）。
    // 返回的 Output 就是模板里 .steps.<id> 能看到的内容。
    Run(ctx context.Context, env Env, c *Ctx) (Output, error)
}

// 有专属配置的节点，再实现这个可选接口（对应 pluginkit 的 New(cfg)）：
type Configurable interface {
    Configure(cfg map[string]any) error
}

// Output 是节点产出的统一形状。和内置节点完全一致，核心统一处理 spill。
type Output struct {
    Text string         // .text（超阈值由核心 spill 到文件，转为 .path）
    Data any            // .json（object/array 时供 dig 取值）
    Meta map[string]any // 额外信息（如 agent 的 session id），模板可读
}
```

### 2.3 为什么 `env` 走 `Run` 而不是构造函数

pluginkit 的 deps 是**装配期注入**（实例图在启动时建好，生命周期内不变）。workflowkit 是**嵌入式库**：宿主可能在每次 `Run` 用不同能力（不同 shell、不同 workspace、测试里换 mock）。

- 若走构造期注入：注册（`init()`）时能力还不存在，且多 Engine/多租户场景下会**共享同一份能力**，与"无全局状态"冲突。
- 走运行期传入：注册只登记"这个 kind 怎么构造"，能力在 `Run` 时给。**代价**是节点拿能力晚一步，**收益**是注册与能力解耦。

这是与 pluginkit 的一处**刻意差异**，需要你确认（§9 待确认 1）。

### 2.4 `Ctx`：节点能看到什么

```go
type Ctx struct {
    StepID string
    Cfg    map[string]any   // 该 step 的专属配置（原始，未渲染）
    Inputs map[string]any   // 调用方传入的参数
    Steps  StepView         // 前序 step 的结果（只读）
    Item   any              // 在 foreach body 内：当前项
    Index  int              // 在 foreach body 内：当前下标
    RunDir string           // 本次运行的产物目录
}

// 模板辅助：节点自己决定渲染哪些字段
func (c *Ctx) Render(s string) (string, error)  // 渲染成字符串
func (c *Ctx) Value(s string) (any, error)      // 渲染并保真 JSON 类型（对象/数组/数字）
func (c *Ctx) Sub(name string) *Ctx             // 派生 ctx（改 StepID/Item 等）

// 组合节点用：执行一组子 step（foreach 就靠它实现）
func (c *Ctx) RunSteps(ctx context.Context, env Env, steps []Step) ([]StepResult, error)
func (c *Ctx) RunStep(ctx context.Context, env Env, step Step) (StepResult, error)
```

**关键分工**：
- **核心负责**：DAG 调度、`when`/`dependsOn`/`retry`/`timeout`、模板渲染、spill、结果聚合、并发屏障。**所有节点类型自动获得这些**，不用自己实现。
- **节点负责**：只做自己那件事（发请求、查库、跑命令、调模型）。它拿到的是 `Ctx` + `env`，不需要知道调度器内部。

### 2.5 内置 5 种节点 = 同一机制的默认实现

| kind | 实现要点 | 需要的 `env` 能力 |
|---|---|---|
| `script` | `c.Render(cfg["run"])` → `env.Shell.Run(...)`；非零退出 → error | `Shell` |
| `tool` | `c.Value(cfg["args"])` → `env.Tools.Execute(...)` | `Tools` |
| `model` | `c.Render(cfg["prompt"])` → `env.LLM.Complete(...)`；可选 `outputSchema` 校验 | `LLM` |
| `agent` | `c.Render(cfg["task"])` → `env.Agents.Run(...)` | `Agents` |
| `foreach` | `c.Value(cfg["items"])` → 逐项 `c.Sub().RunSteps(...)` 跑 `body` | 无（用核心调度） |

它们就在 `nodes/` 下的 5 个包里，各自 `init()` 注册。**删掉任何一个包，库依然可用。**

### 2.6 用户可以定义什么

```go
// 1) 叶子节点：一个新能力
package http
func init() { workflowkit.Register("http", NewHTTP) }

// 2) 组合节点：复用核心调度，组出新模式（saga：正跑失败则跑补偿分支）
package saga
func init() { workflowkit.Register("saga", NewSaga) }
// 内部：c.RunSteps(ctx, env, cfg["steps"]) / c.RunSteps(ctx, env, cfg["compensate"])

// 3) 策略节点：包裹子步骤（条件重试 / 并发 / 超时策略）
package retryuntil
func init() { workflowkit.Register("retryUntil", NewRetryUntil) }
```

**约定**：节点配置里若出现 `body` / `steps` 这类子步骤字段，由**节点自己**决定语义并调 `RunSteps`；核心在 `Parse` 时**递归校验**这些字段里 step 的结构合法性。这样"嵌套"是节点作者的选择，不是核心的负担。

### 2.7 与 pluginkit 的一处必须回避的坑：未知字段

pluginkit 的 config 解码用 `json.Decoder.DisallowUnknownFields()`（`build/decode.go:33`）→ **多一个字段就报错**。这在本库直接冲突：

定义文件是**扁平**的（`{"id","type","when","url","method",...}` 混在一起），核心必须把非公共字段交给节点。若核心也 DisallowUnknownFields，自定义类型的字段必然解析失败。

**取舍**（这个是设计里最需要你确认的一点）：

| 方案 | 做法 | 代价 |
|---|---|---|
| **a. 宽松 + 节点自检**（推荐） | 核心把未知字段收进 `rest map[string]any` 交给节点；节点用 `Configure` 解码时**自己 DisallowUnknownFields** | 核心不报错，错误延后到节点构造 |
| b. 严格 + 声明式 schema | 核心仍严格，但要求节点注册时提供字段白名单（照 pluginkit 的 `Describe` 走反射从 Config struct 推导） | 需要在 `Register` 时传 Config 类型，偏离 pluginkit 的极简 |

推荐 **a**：把 pluginkit 的严格性下放到节点内部（节点对自己的 Config 仍 DisallowUnknownFields），核心只保证公共字段严格。这样既保住"拼错字段立刻报错"的体验，又让自定义类型可用。

---

## 2.8 节点自描述（Agent 构图的根基）

**为什么这是核心需求**：后续整个 workflow 可以交给 **Agent 来构造**。Agent 不可能先读 Go 源码；它只能读两样东西：

1. **可选的节点类型 + 每种类型的参数说明**（它是"积木清单"）
2. **定义文件的格式规则**（它是"拼装说明书"）

所以下图这些东西**不是"文档"，是 API**——可由程序读出来（供 `/nodes` 命令、供 Agent 的 tool 描述、供 IDE 提示）。pluginkit 的 `Describe()`/`Template()` 正是这个思路，我们照搬。

### 2.8.1 三个自描述来源

| 来源 | 内容 | 谁用 |
|---|---|---|
| `workflowkit.Describe(kind)` | 节点类型元信息：kind、说明、**参数 schema**、示例 | Agent / 人 |
| 内置节点的 Config struct | 节点作者写的真实类型（`json` tag + `desc` 注释） | 由 Describe 反射出来 |
| `workflowkit.Schema()` | **workflow 定义文件自身的 JSON Schema**（公共字段 + `steps` 数组） | Agent 校验自己生成的定义 |

### 2.8.2 节点元信息长什么样

```go
type NodeInfo struct {
    Kind        string         // "http"
    Description string         // 一句话（Agent 选路用）
    Config      map[string]any // 参数 JSON Schema（专属字段）
    Example     map[string]any // 至少一个可直接用的最小示例
    // 子步骤字段名（如 "body" / "compensate"）；核心在 Parse 时对这些字段递归校验
    SubSteps    []string
}

func Describe(kind string) (NodeInfo, bool)
func ListTypes() []string            // 已注册的全部 kind
```

**注册时可以只给 Config 类型，schema 反射推导**（与 pluginkit 一致），也可以直接给 schema：

```go
// 方式 1：结构体推导（推荐，类型即文档）
func init() { workflowkit.Register("http", NewHTTP, workflowkit.WithConfig(HTTPConfig{...})) }

// 方式 2：手写 schema（需要条件字段时）
func init() { workflowkit.Register("http", NewHTTP, workflowkit.WithSchema(schema)) }
```

### 2.8.3 workflow 定义自身的 schema

```go
func Schema() map[string]any   // 完整 JSON Schema：description/inputs/concurrency/steps/result
```

Agent 构造流程：**先 `ListTypes()` + `Describe(kind)` 拿到"积木"和它们的参数，再按 `Schema()` 生成 workflow，用 `Parse()` 自校验后调 `Run()`**。

> 这个闭环很重要：Agent 生成的定义能被**同一个库**在加载期校验，错误信息（含可用 kind 列表）能直接反馈给 Agent 修正。**不需要另写一套校验器。**

### 2.8.4 决定：手写 schema，不做反射

| 方案 | 长处 | 代价 |
|---|---|---|
| 反射推导（pluginkit 做法） | Config struct 即文档，不会漂移 | 需要 `reflect` 遍历、注释解析（pluginkit 靠 `desc` struct tag）；而 workflowkit 是**零依赖**目标，且 Go 注释无法在运行时读到（agentkit 靠源码 AST 实现，代价很大） |
| **手写 schema**（推荐） | 无反射、无 AST、schema 可与示例共置、能表达条件字段（`oneOf`/`if-then`） | 多写一坨，有漂移风险 |

**推荐手写**，并用**测试防漂移**：每个内置节点有一个测试，用 `Describe()` 得出的 schema 校验自己的示例（见验收 15）—— 漂移就被测出来。

---

## 2.9 tool 节点参数（对齐 AgentKit 的 inputSchema / outputSchema）

用户想要"每个 tool 能声明参数，像 AgentKit 那样有 inputSchema / outputSchema"。这是个**好抽象**，但要分清两个不同的东西：

| | **工具自身的 schema**（宿主拥有） | **step 里的 args**（定义文件拥有） |
|---|---|---|
| 输入 | 工具定义里的 `inputSchema` | step 的 `args`，值可以是模板 |
| 输出 | 工具定义里的 `outputSchema` | 无需声明（引擎自动把 JSON 结果放进 `.json`） |
| 谁校验 | **宿主**（在 `Execute` 里） | **核心**（在 `Parse`/执行前对 args 校验） |
| 在哪定义 | 宿主的工具注册处 | workflow 定义文件里 |

### 2.9.1 工具目录（供 Agent 选工具）

宿主把工具注册进 `Env`，库提供**只读目录**供 Agent 查：

```go
type Env struct {
    ...
    Tools  ToolInvoker   // 执行
    Catalog ToolCatalog  // 可选：列出可用工具及其 schema（供 Agent）
}

type ToolInfo struct {
    Name         string         // 模型/定义文件里的名字，如 "ledger__import"
    Description  string
    InputSchema  map[string]any // 参数 schema
    OutputSchema map[string]any // 返回结果 schema（可选）
}

type ToolCatalog interface {
    ListTools(ctx context.Context) ([]ToolInfo, error)
}
```

这样"让 Agent 造 workflow"时，Agent 能拿到：**节点类型清单 + 每种节点参数 + 可用工具及它们的 inputSchema/outputSchema**，三样拼出完整定义。

### 2.9.2 step 的 args 校验

`tool` step 的 `args` 支持**声式的静态部分**校验（模板片段跳过）：

- 不含模板的值 → 直接按 `InputSchema` 校验（加载期就能报错）
- 含模板的值 → 延到运行期渲染后校验（仍比工具侧报错更早，且错误带 step id）
- 工具不存在 → **加载期报错**（若 `Catalog` 已注入）

### 2.9.3 错峰：能用目录就用，没有目录也能跑

`Catalog` 为 nil 时不报错，只是少一层加载期检查（args 在执行时由工具侧校验）。保持"能力可选、缺了不炸"的风格。

---

## 3. 定义格式

### 3.1 一个 workflow

```jsonc
{
  "description": "生成回购账本并写库",          // 必填
  "inputs": { "type": "object", "properties": { "date": { "type": "string" } }, "required": ["date"] },
  "concurrency": 4,                            // 可选，层内并发上限
  "steps": [ /* … */ ],
  "result": "已写库，状态 {{ dig .steps.submit.json \"status\" }}"   // 可选
}
```

### 3.2 一个 step

公共字段（核心处理，所有节点类型共享）：

| 字段 | 说明 |
|---|---|
| `id` | 必填，唯一 |
| `type` | 必填，注册过的节点类型 |
| `when` | 可选，渲染为 `""`/`"0"`/`"false"` 则跳过 |
| `dependsOn` | 可选；不写=等前一个；写 `[]`=并行起步；写 `[a,b]`=等这两个 |
| `continueOnError` | 可选，失败也继续（记 `error`） |
| `retry` | 可选，`{maxTries, waitSeconds}` |
| `timeoutSeconds` | 可选 |

专属字段（节点自己定义，核心按 `NodeType.Config()` 校验）—— 例如 `script` 用 `run`、`tool` 用 `tool`+`args`、自定义 `http` 用 `url`+`method`。

```jsonc
{ "id": "gen", "type": "script", "run": "python3 gen.py {{ .inputs.date | quote }} --out {{ .workflow.runDir }}/l.json" }

{ "id": "fetch", "type": "http", "url": "https://api/x?d={{ .inputs.date }}", "method": "GET",
  "dependsOn": [] }                                  // 并行起步（自定义节点）

{ "id": "each", "type": "foreach", "items": "{{ dig .steps.fetch.json \"rows\" }}",
  "concurrency": 3,
  "body": [ { "id": "save", "type": "tool", "tool": "db__insert",
              "args": { "row": "{{ .item }}" } } ] }  // 组合节点：body 由节点自己执行

{ "id": "submit", "type": "tool", "tool": "ledger__import",
  "when": "{{ eq (dig .steps.check.json \"ok\") true }}",
  "args": { "path": "{{ .workflow.runDir }}/l.json" },
  "retry": { "maxTries": 3, "waitSeconds": 2 } }
```

### 3.3 同一套 struct（Go 用户直接写字面量）

```go
wf, err := workflowkit.Parse(data)   // 或
wf := &workflowkit.Workflow{
    Description: "…",
    Steps: []workflowkit.Step{
        {ID: "gen", Type: "script", Fields: map[string]any{"run": "…"}},
    },
}
```

> 设计选择：`Step` 用 **`Fields map[string]any` 承载专属字段**，而不是给每个内置 type 都开一堆强类型字段。
> 理由：节点类型是开放的，核心不该知道 `http` 有哪些字段。强类型只出现在**节点实现内部**（节点自己把 `Fields` 解到自己的 struct，用 `deps` 式解码，错误信息带上 step id）。
> 收益：**加节点类型不需要动核心结构体**。

### 3.4 谁读什么：Agent 构图的自描述 API

> 详细设计见 §2.8 / §2.9。这里是"给 Agent 的接口"汇总。

```go
types    := workflowkit.ListTypes()               // 1) 有哪些积木（kind 列表）
info, ok := workflowkit.Describe("http")          // 2) 每块积木怎么用（参数 schema + 示例 + 子步骤字段）
tools, _ := env.Catalog.ListTools(ctx)           // 3) 有哪些工具可调（名字 + inputSchema/outputSchema）
schema   := workflowkit.Schema()                  // 4) workflow 定义本身的 schema（供 Agent 生成/自校验）

def, _  := workflowkit.Parse(agentGenerated)     // 5) 同一个库加载期校验，错回给 Agent 修正
res, _  := workflowkit.Run(ctx, def, inputs, env) // 6) 执行
```

**关键是第 5 步**：Agent 生成的定义能被**同一个库**在加载期校验，错误信息（含可用 kind 列表）直接回给 Agent 修正。**不需要另写一套校验器，也不会出现"文档与代码不一致"。**

---

## 4. 表达式（一屏）

**语法**：只有 `{{ ... }}`，只出现在字符串字段里。

**上下文**（全部）：

| 写法 | 含义 |
|---|---|
| `{{ .inputs.date }}` | 调用方参数 |
| `{{ .steps.gen.text }}` | 前一步文本视图（超阈值截断） |
| `{{ .steps.gen.path }}` | 前一步大输出的文件路径（by reference） |
| `{{ .steps.gen.json }}` | 前一步的结构化输出 |
| `{{ .steps.gen.status }}` | `ok` / `error` / `skipped` |
| `{{ .workflow.runDir }}` | 本次运行产物目录 |
| `{{ .item }}` / `{{ .index }}` | 仅 `foreach` body 内 |

**函数**（3 个）：`dig`（逐层取 JSON）、`quote`（shell 安全引用）、`readJSON`（只读 runDir/定义目录下的 JSON，供 object 参数）。

**条件**：用 `text/template` 自带的 `eq/ne/lt/gt/and/or/not`。

**不做**：JS 表达式引擎、`$json` 风格、JMESPath、per-item 求值、sprig、任意文件读。

**JSON 保真**：整段渲染为 object/array → 解析成对象传参；整段单一插值且是数字 → 保持字面量（>2^53 不变形）；静态 `"42"` 仍是字符串。

---

## 5. 执行语义

| 项 | 行为 |
|---|---|
| 顺序 | 默认串行；`dependsOn` 组 DAG；同层并行（上限 `concurrency`，默认 4） |
| 并发安全 | 同层只读更早层结果；层间是屏障，结果层结束后统一写回 |
| 跳过 | `when` 为假 → `skipped` |
| 失败 | 默认中止整轮（同层 sibling 跑完、不再开新层）；`continueOnError` 则记录并继续 |
| 重试 | `retry.maxTries` 次，间隔 `waitSeconds`；超时/取消不重试 |
| 超时 | 每 step `timeoutSeconds`（`agent` 默认 900）；超时记 `error` |
| 取消 | ctx done → 中止当前 step，不再开新层，已完成结果保留在 `Result` |
| 超时估算 | 纯函数 `wf.EstimateDuration()` = 关键路径 Σ step 超时；库不设外层墙钟（宿主的事） |

```go
type Result struct {
    Status string                // ok / error
    Text   string                // result 渲染结果（可空）
    Steps  map[string]StepResult // 每步 status/耗时/text/json/path/meta/error
    Err    error
}
```

---

## 6. 宿主能力注入

```go
type Env struct {
    Shell  Shell        // script：Run(ctx, cmd, timeout) (stdout, stderr string, exit int, err error)
    Tools  ToolInvoker  // tool：  Execute(ctx, Call) (Result, error)
    LLM    Completer    // model： Complete(ctx, model, prompt, schema) (string, error)
    Agents AgentRunner  // agent： Run(ctx, agent, task, timeout) (Summary, error)
    Files  Files        // 产物目录：RunDir(runID) (string, error)
    Events EventSink    // 可选，nil 静默
    Traces Tracer       // 可选，nil 静默
    Extra  map[string]any  // 自定义节点带新能力的口子（见 §9 待确认 5）
}
```

- 每个 interface 只 1 个方法（`Files` 2 个），宿主适配成本极低。
- **自定义节点也能用这些能力**：`Run(ctx, wf, inputs, env)` 里的 `env` 直接传给节点。所以用户加一个"调 SQL"的节点，不需要宿主再注入新东西（除非真的要新能力 —— 那时用户在宿主侧自己实现为 interface 并在 `env` 里带进去）。
- 用到而没注入时，报错带 step id + 缺失能力名。

---

## 7. 非功能需求

| # | 要求 |
|---|---|
| NFR-1 | 核心只依赖标准库 + JSON Schema；`go list -deps` 无宿主/网络/DB 包 |
| NFR-2 | 除**类型注册表**（进程级、只读、`init` 后不变）外无全局状态；`Run` 参数自带全部执行上下文，同进程内可并发跑多个 workflow |
| NFR-3 | `-race` 干净；同层并行无数据竞争 |
| NFR-4 | 同输入 + 同 mock 宿主 → 同顺序同结果 |
| NFR-5 | 加载期报错：非法定义、未知 type、节点 config schema 不符、工具不存在、无模板的 args 不符，全在 `Parse` |
| NFR-6 | 错误不含大 payload；`Result.Steps` 可拿到每步 status |
| NFR-7 | **API 面小**：`Register`/`Parse`/`Run`/`ListTypes`/`Describe`/`Schema` 六个函数 + 接口 |
| NFR-8 | **加节点类型不动核心**：新增类型只需实现 `Node`（+ 可选 `Configurable`）并注册 |
| NFR-9 | **自描述不漂移**：每个内置节点有测试，用 `Describe()` 的 schema 校验自己的 Example（漂移即测失败） |
| NFR-10 | **Agent 可用**：`ListTypes`/`Describe`/`Schema`/`Catalog` 四者输出的内容足够 Agent 独立拼出合法定义，无需读源码 |

---

## 8. 验收标准

1. `Parse` 加载期报错覆盖：缺 description、step 无 id、id 重复、未知 type、`when` 语法错、`dependsOn` 引用不存在、成环、节点 config schema 不符
2. 未知 type 的报错**列出已注册类型**
3. 重复注册 **panic**（与 pluginkit 一致）；无 `Replace`（`init()` 阶段就该暴露冲突，不做静默覆盖）
4. 内置 5 种节点用 mock 宿主全部跑通；缺能力时错误含 step id + 能力名
5. **自定义节点**：注册一个测试用节点，能被定义文件引用、能读 `Ctx` 上下文、能渲染模板、能用 `Run` 传入的 `env`
6. **组合节点**：自定义节点内调 `RunSteps` 能执行子 step，子 step 的 `when`/`retry`/`timeout` 照常生效
7. 串行/`dependsOn`/`dependsOn: []`/`when`/`continueOnError`/`retry`/`timeoutSeconds` 行为符合 §5
8. 同层并行（计时断言）+ `concurrency` 上限 + `-race` 干净
9. 大输出走 `.path`，`Result.Text` 不含大文本
10. 整数 >2^53 经 `.json` → `dig` → tool args 三跳不变形
11. `readJSON` 只允许 runDir/定义目录（含 symlink 逃逸被拒）
12. ctx 取消：不再开新层，已完成 step 的 status 仍在 `Result.Steps`
13. `EstimateDuration()` 与关键路径一致
14. 零依赖检查：核心包不 import 宿主包
15. **自描述**：`ListTypes()` 包含全部已注册 kind；`Describe(kind)` 返回非空 Description + Config schema + Example；**用返回的 schema 校验自己的 Example 必须通过**（内置节点逐个体测）
16. **`Schema()` 可用**：每个内置节点的 Example 拼成的最小 workflow 能通过 `Schema()` 校验
17. **Agent 构图闭环**：仅凭 `ListTypes` + `Describe` + `Schema` + `Catalog` 的输出**手工拼**一个合法 workflow，`Parse` 必须成功
18. **tool step 校验**：`Catalog` 已注入时，工具不存在 → `Parse` 报错；无模板的 `args` 不符 `InputSchema` → `Parse` 报错；含模板的延到运行期校验
19. `Catalog` 为 nil 时不报错，只是不做加载期 args 校验

---

## 9. 待确认（4 项，均有默认值）

| # | 问题 | 我的建议 |
|---|---|---|
| 1 | **能力注入时机**：走 pluginkit 的构造期 `deps`，还是运行期 `env` 参数？ | **运行期 `env`**。理由见 §2.3：pluginkit 的 deps 是装配期实例图，而本库是嵌入式、能力随每次调用变（多租户/测试 mock）。**代价**：与 pluginkit 形态不完全一致 |
| 2 | **未知字段策略**：定义文件里节点专属字段怎么处理？ | **方案 a**：核心把未知字段收进 `Fields` 交给节点，节点自己 `Configure` 时 `DisallowUnknownFields`。保住拼错字段报错的体验，又让自定义类型可用（见 §2.7） |
| 3 | **内置节点要不要单独的包**？ | **要**：`nodes/script`、`nodes/tool`… 各自 `init()` 注册。这样"内置"与"自定义"在结构上完全对称，用户可照抄一个内置包当模板 |
| 4 | 组合节点的子步骤字段（`body`/`steps`）由核心递归校验吗？ | **核心递归校验结构合法性**（`[]Step` 的 id/type 等），语义交给节点 |
| 5 | 自定义节点要带**全新能力**（如 DB 连接池）时怎么拿？ | `Env.Extra map[string]any` 开一个口子；或让宿主自定义 `Env` 实现（本库保持 core 的 Env 可嵌入扩展）。具体形式待定 |
| 6 | 节点参数 schema 手写还是反射推导？ | **手写 + 测试防漂移**（见 §2.8.4）。Go 注释运行期读不到，反射+AST 会把零依赖目标破坏掉 |

---

## 附：v0.2 → v0.3 的改动

| 项 | v0.2 | v0.3 |
|---|---|---|
| 节点类型 | 固定 5 种 | **开放注册**，内置 5 种只是默认包 |
| 注册方式 | 无 | **包级 `init()` + kind 字符串 + 构造函数 `(T,error)`**（照搬 AgentKit/pluginkit） |
| 扩展方式 | 无 | `Node`（+ 可选 `Configurable`）两个接口 + `Register` |
| 能力注入 | 无 | **运行期 `env` 参数**（与 pluginkit 的构造期 deps 刻意不同） |
| 组合能力 | 只能靠 `foreach` | 任何节点都能用 `Ctx.RunSteps` 组子步骤（`foreach` 自己就是这么实现的） |
| `Step` 结构 | 内置类型强类型字段 | `Fields map[string]any`，加类型不动核心 |
| 校验 | 核心硬编码规则 | 核心查公共字段 + 节点自检专属字段 |
| 运行时取值 | 核心渲染后传入 | 节点用 `Ctx.Render` / `Ctx.Value` 自行决定渲染哪些字段 |
| API 面 | `New`/`Parse`/`Run` | `Register`/`Parse`/`Run`/`ListTypes`/`Describe`/`Schema`（**无 Engine 对象**） |
| 自描述 | 无 | `Describe` + `Schema` + `Catalog`，**供 Agent 构图** |
| tool 参数 | 只透传 | `args` 按工具的 `InputSchema` 校验（加载期能做到的就加载期） |
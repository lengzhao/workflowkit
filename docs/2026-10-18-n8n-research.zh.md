# n8n / Argo 等工作流引擎调研（借鉴输入）

> 用途：为 workflowkit 的设计提供外部参照。每条结论都尽量给出来源；未能核实的明确标注。
> 抓取时间：2026-10-18。n8n 文档站点提供 `llms.txt` 索引且**每个页面追加 `.md` 即为 Markdown 原文**（如 `https://docs.n8n.io/build/flow-logic/handle-errors-gracefully.md`），这是本文最主要的引用来源。

---

## 1. n8n

### 1.1 工作流 JSON 结构

一个导出的 workflow JSON 顶层只有三个键：`meta` / `nodes` / `connections`（实测自官方模板 API <https://api.n8n.io/workflows/templates/1747>）。

`nodes` 是**数组**（数据结构定义见源码 `INode`，<https://raw.githubusercontent.com/n8n-io/n8n/master/packages/workflow/src/interfaces.ts>，L1751）：

```ts
export interface INode {
  id: string;
  name: string;            // 连接表按 name 索引 → name 必须唯一
  typeVersion: number;     // 节点实现版本，见 §1.8
  type: string;            // 'n8n-nodes-base.code' …
  position: [number, number];   // 仅画布坐标
  disabled?: boolean;
  notes?: string;               // UI 便签
  retryOnFail?: boolean;
  maxTries?: number;
  waitBetweenTries?: number;
  alwaysOutputData?: boolean;
  executeOnce?: boolean;
  onError?: OnError;            // 见 §1.3
  continueOnFail?: boolean;     // 遗留写法
  parameters: INodeParameters;
  credentials?: INodeCredentials;
  webhookId?: string;
  // …
}
```

`connections` 是**按节点名索引的邻接表**（源码 L516–L532）：

```ts
export type  NodeInputConnections = Array<IConnection[] | null>;
export interface IConnection { node: string; type: NodeConnectionType; index: number; }
export interface INodeConnections { [inputName: string]: NodeInputConnections; }  // 如 "main"
export interface IConnections     { [nodeName: string]:  INodeConnections; }
```

语义：`connections["A"].main[outIdx][k] = {node:"B", index:inIdx}` 表示「A 的第 `outIdx` 个 main 输出 → B 的第 `inIdx` 个 main 输入」。**多输出节点**（IF 的 true/false、Switch 的多路、错误输出）就是靠 `main[0]` / `main[1]` 这两层数组表达的：外层数组下标 = 输出口序号。

连接类型不止 `main`：源码 `NodeConnectionTypes`（L3013）还定义了 `ai_languageModel`、`ai_tool`、`ai_memory`、`ai_vectorStore` 等 **AI 子节点连线**，属于 n8n AI 节点体系，与普通数据流分开。

值得注意的是**连接的"扇出"很廉价**：实测模板里 `On clicking 'execute'` 的一个输出口直接连到 6 个下游节点（同一 `main[0]` 数组里 6 个目标），形成 6 路并行分支。

### 1.2 数据模型：items 数组

所有节点间数据都是 **对象数组**，形如（<https://docs.n8n.io/build/work-with-data/understand-n8ns-data-structure.md>）：

```json
[ { "json": { … }, "binary": { "key": { "data": "<base64>", "mimeType": …, "fileName": … } } } ]
```

源码里就是 `INodeExecutionData`（L1865）：`json: IDataObject`（必填）、`binary?`、`error?`、`pairedItem?`、`metadata?`。

关键点：
- **节点默认自动对每个 item 执行**（"nodes process multiple items automatically … performs the configured operation for each one"）。所以 n8n 里的"循环"多数时候是隐式的，`Loop Over Items` 节点只用于需要显式分批/分页的场景（<https://docs.n8n.io/build/flow-logic/loop.md>）。
- 存在两种执行模式（Code 节点参数，`runOnceForAllItems` / `runOnceForEachItem`），Execute Sub-workflow 节点也用同一对概念（"Run once with all items" / "Run once for each item"，<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executeworkflow.md>）。
- **item 之间不保留隐式顺序关系**，跨节点对应关系靠 `pairedItem` 显式记录（源码 `IPairedItemData`：`{item, input?, sourceOverwrite?}`，L1802；`ISourceData`：`{previousNode, previousNodeOutput?, previousNodeRun?}`，L3660），文档见 <https://docs.n8n.io/build/work-with-data/reference-data/link-data-items.md>。

### 1.3 错误处理模型

节点级 `onError` 是三选一枚举（源码 L1750）：

```ts
export type OnError = 'continueErrorOutput' | 'continueRegularOutput' | 'stopWorkflow';
```

含义：
- `stopWorkflow`：整条工作流失败（默认）
- `continueRegularOutput`：出错后仍把**原输入**继续往下传（旧名 `continueOnFail`）
- `continueErrorOutput`：**把错误送到该节点的第二个输出口**，引擎会尝试从 `item.error` 或 `item.json.error` 读取 `message` / `details`（源码 L1879 注释）——这样补偿分支可以接在错误口上

工作流级还有一个独立的 **Error Workflow**：在 Workflow Settings 里指定一条（必须以 **Error Trigger** 节点开头）工作流，主工作流失败时触发，其收到的载荷是（<https://docs.n8n.io/build/flow-logic/handle-errors-gracefully.md>）：

```json
[{ "execution": { "id": "231", "url": "…", "retryOf": "34",
                  "error": { "message": "…", "stack": "…" },
                  "lastNodeExecuted": "Node With Error", "mode": "manual" },
   "workflow": { "id": "1", "name": "Example Workflow" } }]
```

另外有 **Stop And Error** 节点，可以按你设定的条件主动抛错（Error Message 或 Error Object 两种操作），从而触发 Error Workflow（<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.stopanderror.md>）。

### 1.4 控制流 / 编排节点

- **IF / Switch**：分支（<https://docs.n8n.io/build/flow-logic/split-with-conditionals.md>）
- **Merge**：合流，`Mode` 有 Append / Combine（Combine By：Matching Fields 的 inner/outer/left/right join、Position 按序配对、All Possible Combinations 笛卡尔积）/ SQL Query（1.49+），且**"会等所有连接输入的节点执行完毕"**（<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.merge.md>）
- **Loop Over Items（原 Split in Batches）**：按 Batch Size 每轮吐一批走 `loop` 输出，结束后把处理过的数据汇总从 `done` 输出；有 `Reset` 语义（<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.splitinbatches.md>）。文档明确警告：用它做分页循环时**必须有终止条件，否则死循环**。
- **Wait**：After Time Interval / At Specified Time / On Webhook Call / On Form Submitted；等待时**执行数据落到数据库**，恢复条件满足后重新载入继续（<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.wait.md>）
- **Execute Sub-workflow**：Source 可以是 Database（按 ID）/ Local File / Parameter（内联 JSON）/ URL；有 `Wait for Sub-Workflow Completion` 开关；子流程可用 `Define using fields below` / `Define using JSON example` / `Accept all data` 三种输入模式声明契约（<https://docs.n8n.io/integrations/builtin/core-nodes/n8n-nodes-base.executeworkflow.md>）
- 还有 **Filter**、**Edit Fields (Set)**、**Code**、**Common events** 等（llms.txt 索引）

### 1.5 表达式

- 语法是 `{{ … }}`，且源码里有专门的 `ExpressionString = \`={{${string}}}\`` 类型（Interfaces.ts L3066）——**参数以 `=` 开头即表示"这是表达式"**，否则按字面量处理。
- 表达式求值是**逐 item** 的：`$json` 是"当前 item 的 json"（`$input.item.json` 的简写），`$itemIndex` 是当前项下标（<https://docs.n8n.io/build/work-with-data/transform-data/expression-reference/root.md>）。
- 可用根变量/函数（同一页）：`$json`、`$binary`、`$input`、`$itemIndex`、`$execution`、`$nodeVersion`、`$workflow`、`$now`/`$today`、`$env`、`$vars`、`$jmespath(obj, expr)`、`$if(cond, a, b)`、`$ifEmpty(v, fallback)`、`$max/$min`、`$fromAI(key, …)`、`$(nodeName)`。
- 跨节点取值有两种风格：新式 `$("NodeName").first() / .item / .all() / .last()`，旧式 `$node["Name"].json`（<https://docs.n8n.io/build/work-with-data/transform-data/expression-reference.md> 的速查表）。
- 官方建议：复杂变换优先用 **Edit Fields (Set)** 节点，而不是把大表达式散落在各节点参数里。

### 1.6 执行、观测与调试

- 三种执行类型（<https://docs.n8n.io/build/understand-workflows/understand-executions/types-of-executions.md>）：**手动执行**（画布点执行，测试用）、**部分执行**（选中某节点 → Execute step，会顺带把它的上游必要节点跑掉，便于"用同样的输入重跑这一步"）、**生产执行**（触发式）。
- **Pin data**：冻结某节点的输出数据，之后运行不再执行该节点、直接用固定数据往下走；**生产执行忽略所有 pinned data**。
- Settings 里的相关项（<https://docs.n8n.io/build/manage-workflows/configure-workflow-settings.md>）：执行顺序 v0/v1、Error Workflow、谁能调用本工作流（Caller policy）、时区、保存成功/失败执行、**保存执行进度**（"Save"时出错可从断点恢复）、**Timeout Workflow**（超时取消）。
- 执行顺序（<https://docs.n8n.io/build/flow-logic/understand-execution-order.md>）：v0（1.0 前）逐层轮转；v1 逐分支跑完再跑下一分支，**分支顺序由画布坐标决定（上→下，同高左→右）**。这是一个"语义依赖 UI 数据"的坏味道，我们不该学。
- 运行记录结构：源码 `ITaskData`（L3647）含 `executionIndex`、`executionTime`、`executionStatus`、`data`、`error`，以及 `inputOverride`；`ITaskStartedData.source: Array<ISourceData|null>` 记录每路输入的来源。

### 1.7 凭据与导出

导出 JSON **包含凭据名与 ID**（"IDs aren't sensitive, the names could be"），提醒分享前清除；HTTP Request 节点从 cURL 导入时可能带认证头（<https://docs.n8n.io/build/manage-workflows/export-and-import.md>）。凭据实体本身独立存储（`ICredentialsEncrypted` / `ICredentialsDecrypted`，Interfaces.ts L212/L224）。

### 1.8 版本化

- **`typeVersion` 是节点实现的版本号**，同一节点可在不同工作流里用不同版本共存（迁移靠节点自定义 `version` 数组 + `defaultVersion`，n8n 侧约定，本文未逐条核实其文档页）。
- **`executionOrder: 'v0' | 'v1'`、`engineType: 'v1' | 'v2'`** 是工作流级设置（源码 `IWorkflowSettings` L4107）。
- 工作流 JSON 本身没有显式 `schemaVersion` 字段（据导出的顶层键推断）；导入导出有 CLI / API / n8np 包三种通道。

---

## 2. Argo Workflows（K8s，YAML）

- 条件：`when: "{{steps.flip-coin.outputs.result}} == heads"`，底层是 **govaluate** 表达式，支持 `&& || =~` 等（<https://argo-workflows.readthedocs.io/en/latest/walk-through/conditionals/>）。
- **Enhanced depends**（<https://argo-workflows.readthedocs.io/en/latest/enhanced-depends-logic/>）：`depends: "(task-2.Succeeded || task-2.Skipped) && !task-3.Failed"`，结果枚举 `Succeeded / Failed / Errored / Skipped / Omitted / Daemoned`；不写结果等价于 `Succeeded || Skipped || Daemoned`。
- **输出参数**：`outputs.parameters[].valueFrom.path`（从文件取）或 `valueFrom.expression`；下游用 `{{steps.X.outputs.parameters.name}}`，跳过/省略时可用 `valueFrom.default`（<https://argo-workflows.readthedocs.io/en/latest/walk-through/output-parameters/>）。
- **artifact 传递**：`outputs.artifacts[].path` → 下游 `inputs.artifacts[].path`，即**按引用传文件**（<https://argo-workflows.readthedocs.io/en/latest/walk-through/artifacts/>）。
- **循环**：`withSequence{count}` / `withItems[...]` / `withParam`（JSON 数组，可由上游步骤生成，从而"动态工作流"）（<https://argo-workflows.readthedocs.io/en/latest/walk-through/loops/>）。
- **retryStrategy**（<https://argo-workflows.readthedocs.io/en/latest/walk-through/retrying-failed-or-errored-steps/>）；**onExit**：无论成功失败都执行，`{{workflow.status}} ∈ {Succeeded, Failed, Error}`（<https://argo-workflows.readthedocs.io/en/latest/walk-through/exit-handlers/>）；**activeDeadlineSeconds** 限制整workflow 时长（<https://argo-workflows.readthedocs.io/en/latest/walk-through/timeouts/>）。

> 对 workflowkit 的启示：Argo 的 `depends` 结果感知 + `onExit` + `retryStrategy` 是「声明式 + 强静态可读」的路线，比 n8n 的 `onError` 更表达力强；但 Argo 是 YAML + Kubernetes 资源模型，运行时依赖过重。**取语义，不取实现。**

---

## 3. 其他引擎（简述，均来自官方文档）

| 引擎 | 图定义 | 数据传递 | 备注 |
|---|---|---|---|
| **Temporal** | 代码优先（Go/Java/TS 写 workflow 函数），非声明式图 | 函数参数/返回值，走事件历史 | 靠**确定性重放**恢复，语言 SDK 内建；不是"文件定义"模型 |
| **Airflow** | Python DAG，`>>` 连边，任务为 Operator | XCom 传小值，大文件走共享存储 | 调度器 + 元数据库重，偏批处理 |
| **Argo** | YAML `spec.templates` + `steps`/`dag` | 参数 + artifact（§2） | K8s 原生，pod 即任务 |
| **GitHub Actions** | YAML，`jobs.<id>.needs` 表达依赖，`if` 条件 | `outputs`（步骤输出）、artifact 上传下载 | `needs` 只表达"等待"，无结果枚举 |
| **Dify / Coze / LangFlow** | 节点图（LLM 编排倾向），有可视化编辑器 | 节点间变量引用（`{{#node.field#}}` 之类） | 面向 prompt/LLM 链路，控制流较弱 |
| **Zapier** | 线性 trigger→action 序列（+ 少量 filter/path） | 步骤输出字段引用 | 表达能力最弱，胜在生态 |

（Temporal / Airflow / GitHub Actions / Dify / Zapier 的具体字段这里只做对比，未逐页取原文引用。）

---

## 4. 可信度说明

- **已核实（源码级）**：n8n `INode` / `IConnections` / `INodeExecutionData` / `IPairedItemData` / `ISourceData` / `ITaskData` / `IWorkflowSettings` / `OnError` / `NodeConnectionTypes` / `ExpressionString`，以及 1747 号模板的真实 JSON 结构。
- **已核实（官方文档）**：n8n 的错误处理、执行顺序、Wait/Merge/Loop/Execute Sub-workflow 节点、表达式根变量、导出导入、workflow settings；Argo 的 conditionals / enhanced depends / loops / output parameters / artifacts / retry / onExit / timeouts。
- **未逐条核实**：n8n `typeVersion` 迁移机制细节、`n8np` 包格式、AI 子节点连线的运行时行为；Temporal / Airflow / Dify / Zapier 的字段级细节（仅有总体印象）。
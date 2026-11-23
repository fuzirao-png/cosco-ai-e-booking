# COSCO & SMU AI 创新企业项目 · E-Booking Growth Agent

基于 **React + FastAPI + SSE** 的货代客户外呼工作台。选择演示线索，模拟一通电话，从企业注册和资质认证一路完成查价、订舱预览和二次确认。可在界面查看工具执行、知识引用与跨通话画像。

本仓库是按项目描述独立实现的演示工程。内置客户、运价和订舱均为合成数据。外呼使用浏览器麦克风与播报，业务状态写入本地 SQLite。真实电话号码、运营商语音线路与企业 E-Booking API 需要单独对接。

## 本地运行

需要 Python 3.11+ 与 Node.js 22.12+。以下命令从仓库根目录运行。

```bash
python -m venv .venv
# Windows PowerShell
.venv\Scripts\Activate.ps1

python -m pip install -r requirements.txt
npm ci --prefix frontend
npm run build --prefix frontend
python -m uvicorn growth.main:app --host 127.0.0.1 --port 8000
```

macOS / Linux 使用 `source .venv/bin/activate` 激活环境。打开 [外呼工作台](http://127.0.0.1:8000) 或 [API 文档](http://127.0.0.1:8000/docs)。开发前端时，在另一终端运行 `npm run dev --prefix frontend`，打开 [Vite 开发服务](http://127.0.0.1:5173)。

### 演示流程

1. 选中「海岚国际货运」，点击「开始通话」。
2. 点击「企业注册」和「提交认证」，依次发出确认指令。
3. 点击「查询运价」，应看到上海至新加坡 40HC × 2 箱，基础海运费 USD 1,380。
4. 点击「申请订舱」获取预览，再点击「确认订舱」取得本地订舱编号。用同一预览编号重复确认时，返回同一订舱编号。
5. 用「远帆物流」模拟卡点，输入「我卡在认证材料没准备好。」后结束通话。等画像更新后再次开始通话，可在问候中听到卡点，也可在「客户画像」查看原话。

语音演示从输入框旁开启麦克风和播报。浏览器的 [SpeechRecognition](https://developer.mozilla.org/en-US/docs/Web/API/SpeechRecognition) 支持范围因平台而异，语音可能交由浏览器供应商识别。语音不可用时可使用文字输入和「打断回复」按钮。

## 架构

```text
浏览器文字 / ASR 定稿 → POST /turns → 意图路由 → 业务 Skill / 知识库
                            ↓                       ↓
ASR 中间文本 → POST /interrupt → epoch 失效旧轮次    SSE 工具结果与文本增量
                                                    ↓
                                          React 增量渲染 / TTS 分句队列

结束通话 → SQLite 画像队列 → 异步 worker → Pydantic 增量和原话证据 → 下次通话加载
```

| 模块 | 实现 |
| --- | --- |
| 流式语音 | 浏览器可在播报期间监听。语音起点清空 TTS 队列、取消旧 SSE 请求，中间文本交给后端并推进轮次版本。只有最终识别文本进入决策。 |
| 业务 Skill | 每个 Skill 声明触发词及必要槽位。查询运价支持起运港、目的港、箱型和箱量分轮补齐。注册、认证和订舱遵守状态与明确确认条件。SQLite 唯一键使订舱确认可重试。 |
| 知识库 | 章节切分保留港口、箱型、来源标识。BM25 与向量并行排名，经 RRF（k = 60）融合、去重后返回最多八条。模型知识回复使用有字符上限的检索证据。 |
| 跨通话画像 | 基于合成线索初始化。后台从已完成话语中提取卡点并与已验证的业务增量合并。按通话序和话语序覆盖，保留原话、版本和修订号。 |
| 隐私和业务约束 | 敏感词命中时拦截并跳过模型调用与画像保留。拒绝继续外呼会写入停止联系状态。识别到受限货物时冻结当次通话中的演示订舱。 |

### 可选模型和 BGE

默认配置无密钥可运行。将 `.env.example` 复制为 `.env`，填入支持 Chat Completions 的 `GROWTH_MODEL_BASE_URL`、`GROWTH_MODEL` 与服务所需的 `GROWTH_MODEL_API_KEY`。明确的业务指令直接交给 Skill，未匹配的问题可用模型 Function Calling 选只读工具。模型生成的知识回复使用流式增量，提交和确认始终由本地业务状态校验。

使用 BGE 时，安装 `python -m pip install -r requirements-bge.txt` 并设 `GROWTH_EMBEDDINGS=bge`。首次启动需获取 BGE 中文模型权重。默认向量为确定性的本地字符特征，便于离线运行和 CI。

服务按单进程运行，活动会话保存在进程内，已提交订舱、画像与待处理任务存入 SQLite。语音依赖浏览器，评测门禁覆盖服务端轮次失效。真实电话外呼的链路延迟和模型评分需在对接环境中采集。

## 评测

```bash
python -m pip install -r requirements-dev.txt
python -m pytest -q
python -m evals.run --check
```

- 航线 × 箱型 × 箱量 × 问法生成报价变体，加入缺参、越界、危险品、错误确认号、提示注入等案例。CI 校验意图、工具结果、报价金额及 Recall@8 门槛。
- qrel 为单独标注的演示问题集合，相关性由来源 ID 判定。Recall@8 对每个查询的召回比例取宏平均。
- 可选 LLM Judge 以 1–5 分评价目标完成度、事实可靠性、沟通体验、安全性和工具决策。配置 `GROWTH_JUDGE_MODEL`，用同一批样本做人评，并对每个维度计算带并列秩的 Spearman 相关系数。

```bash
python -m evals.run --sample 24 --sample-csv human-ratings.csv
# 将人工 1–5 分填入评分列后运行
python -m evals.run --sample 24 --judge --human-csv human-ratings.csv
```

原项目描述中的 P90、工具调用成功率、画像复用率及人工评分仅为背景。本仓库发布的验证结果来自上面的本地演示评测。

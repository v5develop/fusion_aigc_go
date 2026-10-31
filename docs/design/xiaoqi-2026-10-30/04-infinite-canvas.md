# infinite-canvas 调研（Agent I 产出，浓缩）

## infinite-canvas（/Users/apple/Project/infinite-canvas）
- 开源 basketikun/infinite-canvas，MIT，v0.20.0；React 19 + Vite + zustand + localforage，自研 DOM 画布（非 React Flow）。README 要求二开保留作者信息与页面标识。
- 无业务后端：AI 请求浏览器直连用户配置的 OpenAI 兼容渠道（/v1/images/generations、/v1/images/edits、chat、video、tts；Gemini 原生）。canvas-proxy 仅本机 CORS 透传；canvas-agent 是本机 Codex/Claude CLI 桥（MCP 工具），都不上线。
- 持久化全在浏览器 IndexedDB（库 infinite-canvas：app_state/canvas_store、asset_store、image_files、media_files、prompt_cache），画布 JSON 只存 storageKey。只有 WebDAV 同步与 zip 导入导出。
- 提示词库：7 个远程来源（raw.githubusercontent.com/yukkcat/image-prompts/...），运行时拉取缓存，条数本地数不出；RawPrompt {id,title,prompt,description,coverUrl,referenceImageUrls[],tags[],preview,createdAt,updatedAt,author?,sourceUrl?,imageMode?,imageModel?,imageSize?,imageCount?}；自定义来源只要吐标准 JSON 即可接入。
- 画布类型（web/src/types/canvas.ts）：CanvasNodeType image/text/config/video/audio/group + 插件 "<pluginId>:<name>"；CanvasNodeData {id,type,title,position{x,y},width,height,metadata}；metadata 含 prompt/model/size/count/references[]/images[]/storageKey/videoTaskId...；CanvasConnection {id,fromNodeId,toNodeId}；CanvasProject {id,title,createdAt,updatedAt,nodes[],connections[],chatSessions[],activeChatId,backgroundMode,viewport{x,y,k}}。config 节点=生成配置节点，上游由 connections 推算。
- 替换点：渠道 baseUrl 指向自家 OpenAI 兼容网关即可（需 CORS）；importChannelCredentials({baseUrl,apiKey}) 现成；运行时配置 window.__RUNTIME_CONFIG__ 只有统计 ID，无 API base/token 注入点需改；无 postMessage 宿主协议；插件系统可加自家节点。
- 原型嵌入：public/canvas-app/ 3.7MB 原版构建（basename /canvas-app）；/canvas-official → CanvasEmbedView.vue iframe 同源 /canvas-app/canvas/seed-xiaoqi-2026-10-20；数据通道=Vue 侧直写 IndexedDB 种 seed 项目 + URL query ?image=&title= + localStorage 主题 + contentDocument 点按钮；无 postMessage；仅演示，不可生产。原型 PromptLibraryPanel.vue + data/prompt-library.ts 是 Vue 侧自写（模型 通用/GPT/Banana/Seedream × 电商/通用/我的，约 21 条 mock），与 iframe 画布未打通。
- 提示词辅助系统原型（14 步骤图+2 总览）：零输入选择式提示词生成向导（主体→细化→场景→风格→情绪→镜头→光色→输出→结果 完整/简洁/负面/模型适配 + 变体 A/B/C；视频流程 11–14）。与提示词库不同：是结构化生成器（数据字典 + 冲突规则 + 可选 LLM 润色），输出可存入「我的」提示词库或直接作 config 节点 prompt。需后端配置表支撑。

- 启示：画布 AI 操作（改图/多视角/图层）已有；画布文档（节点/边）存储任何仓库都没有；画布 config 节点 ↔ DSL step 粒度不同需产品判断。

## 待确认
- **新功能落点**：fusion_aigc_go（用户指定，本地生活 go-zero，afv_* 表）；画布文档（节点/边）存储任何仓库都没有，需要在本项目新开（见 `00-需求分流与设计方案.md` §6）。

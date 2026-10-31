# fusion-aigc-web 盘点 + Java creaft v1 兼容层对照（Agent H 产出，浓缩）

## 结论
1. 原型说的宿主（AppLayout + NMenu、/tools/:toolId、/workflow、/local-life、/assets）是**桌面 Electron creation 模块**（refs/前端接入边界 §1-2 `_rea-shell/out/renderer`），不是 fusion-aigc-web。
2. fusion-aigc-web（Vue3 + Naive UI + hash 路由，master）：首页/图片/视频/电商 14 页/资产/登录；只调 Java 原生 /api/v1/creation/*、/asset/*、/aigc/model-options、/billing/quote/preview、/storage/direct/sts；**一个 creaft v1 兼容接口都没调**；自绘 80px 导航 + 电商多标签 keep-alive；无工具箱/工作流/本地生活/画布/任务中心/点数页。
3. **Java fusion-aigc 已有完整 creaft v1 兼容层** controller/compat/v1/*：15 个工具 + /workflows/visual（wf:13/14）+ 工具/工作流历史（含完整请求快照 request/promptOptions/inputRoles/templateUrls/mode）+ 本地生活模板/档案只读 + /features/image-model-configs、/video-model-configs、/biz-points + /files/sts-token + /users/assets + /users/task-collections（任务级收藏）+ /tasks/list。
4. 原型契约自定义的 POST /tasks(target=schemaId)、GET /meta/*、/meta/models、multipart /uploads **Java 和 Go 都没有**。可前端适配层翻译到兼容路由，或新做。
5. 鉴权：网页独立登录已实现（联盟短信 → union-token-login → fusion Bearer，localStorage）；兼容层是否认 fusion Bearer token 待确认。

## 工具 ↔ 兼容路由（CreaftV1CoreToolsController.java）
ai-upscale→/tools/ai-image-upscaler；ai-eraser→/tools/ai-erase(原图+蒙版)；ai-cutout→/api/v1/tools/cutout；image-translate→/tools/ai-image-translate(+/tools/ocr，textRectangles 已有)；text-replace→/tools/ai-text-replace；aspect-ratio-convert→/tools/ai-ratio-convert；image-generate→/tools/ai-image-generator；first-screen→/tools/ai-first-screen-main-image；background-generator→/tools/ai-background-generator；product-retouch→/tools/ai-retouch；product-replace→/tools/ai-product-replace(inputRoles)；cloth-wrinkle→/tools/ai-clothing-dewrinkle；video-enhance→/tools/ai-video-enhance；video-translate→/tools/ai-video-translate；video-generator→/tools/ai-video-generator；posture-fission→/tools/ai-posture-fission；digital-model→/tools/ai-model-tryon。
工作流：POST /workflows/visual；模板 GET /workflows/templates/{category}（payload image_urls[].title/max_num，后台 /admin/creaft/workflow-templates 可改 max_num 1→4）；历史 /workflows/{title}/histories。
本地生活：/local-life-templates/list|categories|{id}；/user-local-life-templates/*；/brand-profiles(/forms,/{id},/{id}/assets)；真提交 /workflows/local-life + /continue 已有。
模型/价格：model 字段在 CoreToolSubmissionRequest；视频按别名路由，图片 ExecutionProfileService.resolveVariantKey；价格 /features/image-model-configs 按用户实时。
提示词上限：兼容层 32000，视频 5000（CompatV1InputChecks）→ 3000 纯前端。
历史快照：CompatV1HistoryView（taskGroupId 可聚合批量）。

## 分类
- 纯前端（后端已有）：F1 壳、F2 组件动效、上传复用 OSS 直传、15 工具页接兼容路由、wf:13/14、历史+快照回填+批量聚合、提示词 3000、本地生活只读、资产列表、任务级收藏、模型价格改用 /features/*、schemaId→兼容路由适配层。
- 前端 + Java 配置：四档模型 variant 别名与定价（逐工具核实）；服装图 max_num 1→4；工具箱分组。
- 需新后端：/meta 远程 schema；统一 POST /tasks + /meta/models（若坚持原型契约）；资产 kind 分类(模特/场景/妆容/发型/配饰/未分类)+计数+资产级收藏+标签字典；生成结果资产化改名（待确认）。

## 待确认
1 落点仓库（桌面 creation vs fusion-aigc-web）；2 线上电商 14 页/图片/视频去留；3 兼容层是否认 fusion Bearer、是否要 request-id 头；4 wf:13/14 的 count/itemPrompts 实际字段名、partial 状态；5 四档模型配置进度、image-model-configs 是按工具还是全局；6 /meta 与 /tasks 要不要；7 资产分类按哪套；8 收藏对象资产级还是任务级。

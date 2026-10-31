# Java fusion-aigc 能力盘点 vs 契约（Agent C 产出，浓缩）

## 结论
1. **17 工具 Java 全有**（creaft v1 能力码），14 工作流模板全有（/workflows/fashion|visual|video + title，afv_compat_workflow_template，种子 V1.0.149）。v1.0 的 15 工具 + 2 工作流**无需 Java 新增生成能力**。差异三类：接口形状（统一 POST /tasks vs 每工具一路由）、批量（契约 ≤20 张 vs Java 多数一次 1 张需前端循环；ratio-convert 服务端拆子任务 ≤20）、硬编码上限（提示词 2000）。
2. **Java 已有本地生活**：brand-profiles CRUD、local-life-templates list/categories/{id}、user-local-life-templates、/workflows/local-life + continue、biz-points.localLife。缺 coverage 和 POST /local-life/price。
3. 通用接口面多数有同类但形状不同；完全没有：/meta/tools/{id}/params schema 下发、游标分页统一历史、批次取消、资产收藏、按分类计数 [{key,label,count}]、单用户并发 ≤4 + 429、契约错误码/data.field/shortfall/retryAfter、图片工具按请求选模型、多件换衣、视频 multipart 上传、用户提示词库、用户画布 nodes/edges 存取。
4. 建议：契约 → Java 兼容路由的翻译放在 Go BFF 或前端 adapter；Java 只动配置/数据；唯一必须改 Java 代码：提示词 2000→3000（CreaftV2Contract.java:207,245，影响场景生成、wf:13、wf:14）。

## 工具映射（POST /api/v1/tools/...，支持 Idempotency-Key，子任务键 key#index）
upscale→ai-image-upscaler(1 张/次, 1k/2k/4k 小写)；eraser→ai-erase(original_image_url, mask_url 需先上传)；cutout→/tools/cutout 同步 ≤50s 或 /api/v2/tools/cutout 异步；image-translate→ai-image-translate(1 张, **只出图不出 textRegions**)；text-replace→/tools/ocr 同步(阿里云 Results[].TextRectangles{Angle,Height,Left,Top,Width,Pos}) + ai-text-replace(text_information[{text,target_text,text_rectangles}], num 1-9)；ratio-convert→ai-ratio-convert(image_urls 1-20 服务端拆子任务同 taskGroupId；resolution+aspect_ratio 或 width+height)；image-generate→ai-image-generator(content, image_urls ≤账号上限默认 4, resolution, aspect_ratio, num 1-9, rewrite_prompt；**无按请求选模型**，model_roles 决定默认 GPT Image 2；提示词无上限)；first-screen→同+language；background-generator(content ≤**2000**, image_url, is_fission, ...)；posture-fission(mode fission/reference/imitation, image_url, reference_image_url, content, ...)；product-retouch→ai-retouch(1 张)；product-replace→ai-product-replace(original_image_url, product_image_url, content；1+1/次)；cloth-wrinkle→ai-clothing-dewrinkle(1 张)；digital-model→ai-model-tryon(cloth_image_url 1, model_image_url 1；多件换衣 V1.0.197 下架；模特 /models/templates、/user-models)；video-enhance(video_url, resolution 720p/1080p/2k/4k, model)；video-translate(video_url/link, language, voice_model, voice_key←/voices/list, add_subtitle, erase_original_subtitle)；video-generator(model 别名 creaft-video-2.5/2.0/2.0-fast/2.0-mini V1.0.165, content ≥5, image_urls, video_url/link, resolution, **ratio**, duration, generate_audio, rewrite_prompt)。
另有 ai-inpaint(局部重绘 images[1-2]{url,bbox}, content, num)、画布四件套 /agent/canvas/{identify,image-edict,multiview,image-layer}。无独立扩图（=比例转换）、无用户裁剪接口（仅工作流内部 MediaCropStepExecutor）、无看图反推、首尾帧只在原生 /generation/video。

## 工作流
wf:13 → /workflows/visual title=visual-style-replace {imageUrl, prdImageUrls[{url,content ≤300}]}，1+N ≤账号上限 4，提示词 2000；itemPrompts[i]↔prdImageUrls[i].content。
wf:14 → visual-sku-replace，产品 ≤20，num ≤4(VISUAL_SKU_MAX_GROUPS)，出图=num×产品数，合成 1 条历史，提示词 2000。
wf:1-5,8,10-12 → /workflows/fashion title=fashion-model-*（默认未启用需运营导入 payload；图片拍平 image_urls；content 必填；wf:5/8 6/5 张超上限 4）。wf:6/7/41 → /workflows/video（契约字段照抄图片套，需修正）。

## 通用接口面
- 上传：POST /api/v1/storage/upload(file, subDir) 只收图片 ≤100MB 返 url；视频走 /storage/direct/{presign,sts,confirm} 或 /files/sts-token。
- 任务：GET /api/v1/tasks/{taskUid} → {taskUid,taskGroupId,status,payload,result,errorCode,errorMessage,createdAt,updatedAt,isCollected}；POST /tasks/list {taskUids,taskGroupIds}；取消 POST /api/v1/creation/{taskId}/cancel（原生，退款；兼容 taskUid 即 AiCreation_ 任务号 CompatTaskService:335，需联调确认）；无批次取消；progress 用 task-center。
- 幂等冲突码 IDEMPOTENCY_CONFLICT 46009。
- 历史：GET /api/v1/{tools|workflows}/{title}/histories（payload 含 request 快照 + inputRoles，可回填；SKU 合成 1 条；分页 current/pages/total）；GET /creation/task-center/tasks 跨入口（不带请求参数）。
- 资产：GET /asset/all(page/pageSize ≤100, keyword, assetCategory, aspectRatio, timeRange, 按任务分组, typeCounts)；GET /asset/categories 两级树无 count；PUT /asset(name/description/tags)；POST /asset；DELETE /asset/{id}；/asset/batch-delete；任务收藏 /users/task-collections；无资产收藏。
- 点数：GET /points/logs、/points/balance；/open/points/records。
- 模型价格：/features/image-model-configs、video-model-configs（[{model,description,size[{resolution,aspectRatio,points}]}] 按用户报价，图片仅一个模型）、biz-points、task-avg-duration/{type}；/api/v2/features 能力分组。
- 限流：@RateLimit 未加在兼容提交路由；无单用户并发限制。
- 信封 CommonResult{code,msg,data,requestId,timestamp} 成功 0；错误码 1003 参数/2001 未登录/2005 无权限/3003|46004 余额不足/3006 任务不存在/46009 幂等冲突/1004 太频繁/6002 AI 失败；业务失败 HTTP 200。
- 鉴权：兼容路由与原生同一 Bearer（TokenAuthenticationFilter @Order(6) /api/v1/**、/api/v2/**）；/auth/login、/refresh、/wx/*、/h5/*。

## 校验位置
账号单次参考图 4：CompatImageCountLimit.java:43 ← SettingKey.COMPAT_V1_MAX_IMAGE_COUNT（DB > yaml CREAFT_MAX_IMAGE_COUNT:4）**全局值**，影响 Agent/帮写/档案图组。v1 请求模型 resources/compat/v1-request-models.json。场景生成 2000 CreaftV2Contract:207；单图复制/SKU 2000 :245；SKU 单品 300 :217；SKU 组数 ≤4 Adapter:206；视频参考 videoReferenceCap Adapter:695；原生 model-presets maxReferenceImages。

## 冻结下的判断
配置/数据：max_image_count 4→9（AdminSettingsController）；启用 wf 模板 + payload（AdminCreaftWorkflowTemplateController、ops/sql/creaft-v1-workflow-template-import.sql）；model_roles/video_model_aliases/价目/afv_compat_feature_catalog/素材分类入口映射/binding enabled（部署前用 CompatBindingAdmission 核对）。
必须改代码：提示词 2000→3000 两行。
不该在 Java 做（放 Go BFF/前端 adapter）：统一 /tasks、/meta/*、/history 游标、批次聚合 partial、错误码信封转换、/uploads 外壳、/local-life/price。
新增需另议：资产收藏、分类计数、并发 ≤4/429、批次取消、按请求选模型、多件换衣、coverage、视频 multipart。

## 拍板
1 本地生活读接口 Java 兼容层 vs Go；2 图片翻译 textRegions 是否必需；3 wf:6/7/41 契约字段修正。

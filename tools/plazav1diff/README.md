# plazav1diff — creaft v1 兼容三接口的 Java / Go 黄金比对

同一组请求同时打 Java 与 Go，规范化后逐字段比较，输出差异表。切流前用它证明 Go 复刻（`app/plaza/v1compat`）与 Java 一致。

被比对的路径：`GET /api/v1/plaza-templates/list`、`GET /api/v1/plaza-templates/tags`、`GET /api/v1/plaza-headlines`。

## 用法

```bash
go run ./tools/plazav1diff \
  -java http://<Java 内网地址>:<端口> \
  -go   http://127.0.0.1:<gateway-api 端口> \
  [-token <用户 access token>] \
  [-report /tmp/plaza-v1-diff.md] \
  [-only 'headlines'] [-retries 2] [-retry-wait 3s] [-timeout 15s] [-corpus tools/plazav1diff/corpus.json]
```

- `-java` / `-go` 也可用环境变量 `PLAZA_DIFF_JAVA` / `PLAZA_DIFF_GO`，`-token` 用 `PLAZA_DIFF_TOKEN`。
- **两边都直连内网**（Java 的 Spring 端口、Go 网关端口），不要经 nginx——切流后 nginx 上同一路径只会到一边。
- `-token` 可选：语料里 `auth: true` 的条目带 `Authorization: Bearer <token>`（验证登录态不影响输出）；不给就跳过这些条目。随便一个测试账号即可，接口不读用户。
- 退出码：`0` 无硬差异；`1` 有硬差异（不能切流）；`2` 运行错误（连不上、语料坏了、Java 取占位符数据失败）。
- 先确认 Go 开关已开（gateway-api `Plaza.V1Compat.Enabled: true`），否则 Go 侧三条都是 404，报告里全是「HTTP 状态不同」。

输出示例：

```
plazav1diff  java=http://10.0.0.5:8080  go=http://127.0.0.1:8888  语料 62 条
[容差] headlines  GET /api/v1/plaza-headlines  （取 1 次）
  ~ data.items[plazaTemplateId=12].uses  java=41  go=43  统计字段（容差内）
[差异] list_tag_single  GET /api/v1/plaza-templates/list?tags=%E7%94%B5%E5%95%86  （取 3 次）
  ! data.data[0].payload  java={"a":1,"b":2}  go={"b":2,"a":1}  原文不一致（键序 / 数字写法 / 转义）
汇总：硬差异 1 条（1 个请求）；容差内 1 条；跳过 2；错误 0
结论：存在硬差异（退出码 1），不能切流
```

`-report` 另写一份 Markdown 表（条目、请求、路径、Java 值、Go 值、说明、判定），可直接贴进上线单。

## 比较口径

| 项 | 规则 |
|---|---|
| HTTP 状态 | 必须相同 |
| 响应体结构 | 按 JSON 树比较；对象键序**不**要求（Java 分页包装是 `Map.of`，键序随 JVM 变）；数组按位置；数字按字面量（`1.0E10` ≠ `10000000000`）；字符串按解码值 |
| `payload` 子树 | **逐字节**比较原文（键序、数字写法、转义都必须一样） |
| 顶层 `timestamp` / `requestId` | 忽略 |
| 统计字段 `usageCount / uses / previousUses / trend / isNew` | 两边各有 10 分钟缓存、相位不同：数字差 ≤ `max(volatileAbs, volatileRel × 较大值)`（默认 5 / 10%）放过；`trend` 变 null、`isNew` 翻转只有同一条目里有容差内的数字差才放过 |
| 头条 `items` | 按 `plazaTemplateId` 对齐后比内容；`rank` 与顺序差异只有本响应里存在容差内统计差异时才放过（`uses` 排序会跟着变） |
| 其余 | 必须相等 |

出现硬差异时会重取（`-retries`，默认 2 次，间隔 `-retry-wait`），取硬差异最少的一次——两边统计缓存刚好在两次请求之间换代时会抖一下，真实的差异每次都会复现。

## 语料（`corpus.json`）

62 条：列表 `page ∈ {1, 2, 末页, 越界页} × size ∈ {1, 10, 24, 100}`，缺省 / 空串 / 十六进制 / 带空白 / 重复传 / `size=0` / `size=1000` / 非数字（400）；标签单个、重复传、逗号分隔（含编码逗号、带空格）、多值不拆、不存在、中文、emoji、空串、分页；未知参数（`industry` 等）忽略；假 token、自带 `X-Request-Id`、真实登录态；标签清单；头条 `tag` / `keyword` 各种组合（大小写互换、空串、空白、不存在、重复传、`industry` 忽略）。

格式：

```json
{
  "rules": { "ignore": [...], "raw": ["payload"], "volatile": [...], "orderDerived": ["rank"],
             "align": {"data.items": "plazaTemplateId"}, "volatileAbs": 5, "volatileRel": 0.1 },
  "requests": [ {"name": "list_tag_single", "path": "/api/v1/plaza-templates/list", "query": "tags={tag0}",
                 "headers": {"X-Request-Id": "..."}, "auth": false } ]
}
```

`query` 里的占位符在运行时先问 **Java** 拿真实数据再填（用基准方的数据造请求），取不到就跳过该条并在报告里写明：

| 占位符 | 取值 |
|---|---|
| `{last:S}` / `{beyond:S}` | `size=S` 时的末页 / 末页 + 1（越界页） |
| `{tag0}` `{tag1}` `{tagLast}` | 标签清单第 1、2、最后一个 |
| `{tagCJK}` `{tagEmoji}` | 第一个含汉字 / 含增补字符（emoji 等）的标签 |
| `{htag}` | 第一条头条的第一个标签 |
| `{hkw}` `{hkwMixed}` | 第一条头条标题的前两个字；及其 ASCII 大小写互换 |

**语料里不放有意偏差的请求**（`page<1` 且 `size≠0`、`size<0`、`page×size` 溢出成负数等：Java 500、Go 400），偏差清单见 `docs/api/api-plaza.md`「v1 兼容三接口」。
想补用例直接往 `requests` 里加；数据相关的边角（停用 / 删除模板、`data_json` 含超长整数与嵌套键序）在预发库里造好再跑。

## 在 t81 / 预发跑

1. Go：发布含 `app/plaza/v1compat` 的 gateway-api，配置加 `Plaza: { V1Compat: { Enabled: true } }`，重启；`curl -s http://127.0.0.1:<网关端口>/api/v1/plaza-templates/tags` 应返回 `{"code":0,...}`。nginx 不用动。
2. 找到 Java 的内网地址（宝塔 nginx 模板里 `upstream aigc_java` 的 `__JAVA_UPSTREAM__`）。
3. 在能同时访问两边的机器上（一般就是 Go 所在机器）：

   ```bash
   cd fusion_aigc_go
   go run ./tools/plazav1diff -java http://<JAVA_UPSTREAM> -go http://127.0.0.1:<网关端口> -report /tmp/plaza-v1-diff.md; echo "exit=$?"
   ```

   机器上没有 Go 工具链就先在本地 `GOOS=linux GOARCH=amd64 go build -o plazav1diff ./tools/plazav1diff`，把二进制和 `corpus.json` 拷过去，用 `-corpus` 指定语料路径。
4. 生产只读影子（切流前门槛）：每 5 分钟跑一次，非 0 告警，例如

   ```bash
   while true; do ./plazav1diff -java ... -go ... -corpus ./corpus.json -report /var/log/plaza-v1-diff-$(date +%H%M).md \
     || echo "plaza v1 diff 失败 $(date)" | <告警命令>; sleep 300; done
   ```

## 怎么据此决定切流

| 结果 | 动作 |
|---|---|
| 退出码 2 | 先排障（地址、Go 开关、Java 是否正常），结果不算数 |
| 退出码 1 | 不切。看报告里的路径：`payload` 原文差异 → 查 Jackson 往返复刻（`pkg/v1wire/javawire.go`）；排序 / 条数差异 → 查 SQL 口径与数据；`HTTP 状态不同` → 查参数绑定或开关；修完重跑到 0 |
| 退出码 0（只有容差项） | 预发跑通后上生产影子；**连续 24 小时每轮都是 0** 再按 `docs/api/api-plaza.md`「切流步骤」改 nginx（10% → 50% → 100%，先 `tags` / `list` 后 `plaza-headlines`） |
| 切流后 | 影子继续跑（两边都直连内网，不受 nginx 影响）；一旦出现硬差异或 Go 侧 5xx 上升，nginx 变量改回 `aigc_java` 回滚 |

## 重新生成 Java 黄金样本

`app/plaza/v1compat/testdata/java_golden.json` 来自 `javagolden/GoldenGen.java`：用 fusion-aigc 编译产物里的真实类
（`CreaftV1SupportController`、`CompatSupportService`、`PlazaHeadlineService`、`PlazaTemplateUsageStatsService`、`CommonResult`）+ Jackson 2.17.3
跑 `testdata/fixture.json`。Java 这几个类有改动、或 fixture 加了用例时重新生成：

```bash
cd fusion-aigc
export JAVA_HOME=$(/usr/libexec/java_home -v 21 2>/dev/null || echo /opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home)
./mvnw -q -DskipTests compile
./mvnw -q -o dependency:build-classpath -Dmdep.outputFile=/tmp/cp.txt -Dmdep.includeScope=runtime
CP="target/classes:$(cat /tmp/cp.txt)"
mkdir -p /tmp/plazav1gen && $JAVA_HOME/bin/javac -proc:none -cp "$CP" -d /tmp/plazav1gen ../fusion_aigc_go/tools/plazav1diff/javagolden/GoldenGen.java
$JAVA_HOME/bin/java -cp "/tmp/plazav1gen:$CP" GoldenGen \
  ../fusion_aigc_go/app/plaza/v1compat/testdata/fixture.json ../fusion_aigc_go/app/plaza/v1compat/testdata/java_golden.json
cd ../fusion_aigc_go && go test ./app/plaza/v1compat/
```

头条日期是相对「生成当天」的偏移，golden 里记了 `nowEpochMilli`，Go 单测用同一时刻，所以哪天跑都稳定。

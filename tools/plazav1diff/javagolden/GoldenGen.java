import com.baomidou.mybatisplus.core.MybatisConfiguration;
import com.baomidou.mybatisplus.core.metadata.TableInfoHelper;
import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.fasterxml.jackson.databind.json.JsonMapper;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fasterxml.jackson.datatype.jsr310.JavaTimeModule;
import com.fasterxml.jackson.datatype.jsr310.ser.LocalDateTimeSerializer;
import com.pengqiwl.fusion.common.BusinessException;
import com.pengqiwl.fusion.common.CommonResult;
import com.pengqiwl.fusion.common.ErrorCode;
import com.pengqiwl.fusion.controller.compat.v1.support.CreaftV1SupportController;
import com.pengqiwl.fusion.entity.compat.v1.support.CompatPlazaTemplate;
import com.pengqiwl.fusion.entity.plaza.PlazaHeadline;
import com.pengqiwl.fusion.mapper.compat.v1.support.CompatPlazaTemplateMapper;
import com.pengqiwl.fusion.mapper.plaza.PlazaHeadlineMapper;
import com.pengqiwl.fusion.service.compat.v1.support.CompatSupportService;
import com.pengqiwl.fusion.service.plaza.PlazaHeadlineService;
import com.pengqiwl.fusion.service.plaza.PlazaTemplateUsageStatsService;
import org.apache.ibatis.builder.MapperBuilderAssistant;

import java.io.File;
import java.lang.reflect.Constructor;
import java.lang.reflect.Proxy;
import java.time.LocalDate;
import java.time.LocalDateTime;
import java.time.ZoneId;
import java.time.format.DateTimeFormatter;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * v1 兼容三接口的 Java 黄金样本生成器：用 fusion-aigc 编译产物里的<b>真实类</b>（CreaftV1SupportController /
 * CompatSupportService / PlazaHeadlineService / PlazaTemplateUsageStatsService / CommonResult）+ Jackson 2.17，
 * 喂 fixture.json 的同一份数据，写出 Java 的响应体到 java_golden.json；Go 单测（app/plaza/v1compat）拿它逐字段比对。
 *
 * <p>Mapper 用动态代理顶替（只返回 fixture 里已按 SQL 口径过滤、排序好的行），因为 SQL 条件不是被测对象——
 * 被测的是 Java 的过滤 / 去重 / 排序 / 分页 / 字段拼装 / Jackson 往返；SQL 口径由 Go 的集成测试对真库覆盖。
 * 异常按 GlobalExceptionHandler 的映射落成 HTTP 状态 + CommonResult.error（BusinessException → httpStatusOf(code)；其余 → 500 SYSTEM_ERROR）。
 *
 * <p>另外输出两组 Java 语义样本：Jackson Map 往返（数字 / 转义 / 重复键）与 String.compareTo 排序。
 * 用法见 tools/plazav1diff/README.md「重新生成 Java 黄金样本」。
 */
public class GoldenGen {

    static final ZoneId ZONE = ZoneId.of("Asia/Shanghai");

    public static void main(String[] args) throws Exception {
        File fixtureFile = new File(args[0]);
        File out = new File(args[1]);
        ObjectMapper om = JsonMapper.builder()
                .addModule(new JavaTimeModule().addSerializer(LocalDateTime.class,
                        new LocalDateTimeSerializer(DateTimeFormatter.ofPattern("yyyy-MM-dd HH:mm:ss"))))
                .disable(SerializationFeature.WRITE_DATES_AS_TIMESTAMPS)
                .build();

        MybatisConfiguration mc = new MybatisConfiguration();
        TableInfoHelper.initTableInfo(new MapperBuilderAssistant(mc, ""), CompatPlazaTemplate.class);
        TableInfoHelper.initTableInfo(new MapperBuilderAssistant(mc, ""), PlazaHeadline.class);

        JsonNode fixture = om.readTree(fixtureFile);
        LocalDateTime now = LocalDateTime.now(ZONE);
        LocalDate today = now.toLocalDate();
        ObjectNode root = om.createObjectNode();
        root.put("comment", "由 tools/plazav1diff/javagolden/GoldenGen.java 生成，勿手改。body 已去掉 requestId / timestamp。");
        root.put("javaVersion", System.getProperty("java.version"));
        root.put("jacksonVersion", com.fasterxml.jackson.databind.cfg.PackageVersion.VERSION.toString());
        root.put("now", now.toString());
        root.put("nowEpochMilli", now.atZone(ZONE).toInstant().toEpochMilli());
        ArrayNode dsOut = root.putArray("datasets");

        for (JsonNode ds : fixture.get("datasets")) {
            List<CompatPlazaTemplate> enabled = new ArrayList<>();
            for (JsonNode t : ds.get("templates")) {
                if (t.get("enabled").asInt() != 1) continue;
                CompatPlazaTemplate row = new CompatPlazaTemplate();
                row.setId(t.get("id").asLong());
                row.setTitle(text(t.get("title")));
                row.setDescription(text(t.get("description")));
                row.setTagsJson(text(t.get("tagsJson")));
                row.setDataJson(text(t.get("dataJson")));
                row.setSortOrder(t.get("sortOrder").asInt());
                row.setEnabled(1);
                row.setUpdateTime(LocalDateTime.parse(t.get("updateTime").asText().replace(' ', 'T')));
                enabled.add(row);
            }
            // SQL：ORDER BY sort_order ASC, update_time DESC, id DESC
            enabled.sort(Comparator.comparing(CompatPlazaTemplate::getSortOrder)
                    .thenComparing(CompatPlazaTemplate::getUpdateTime, Comparator.reverseOrder())
                    .thenComparing(CompatPlazaTemplate::getId, Comparator.reverseOrder()));
            List<PlazaHeadline> active = new ArrayList<>();
            for (JsonNode h : ds.get("headlines")) {
                LocalDate s = today.plusDays(h.get("startOffset").asLong());
                LocalDate e = today.plusDays(h.get("endOffset").asLong());
                // SQL：enabled = 1 AND start_date <= today AND end_date >= today AND deleted = 0
                if (h.get("enabled").asInt() != 1 || s.isAfter(today) || e.isBefore(today)) continue;
                PlazaHeadline row = new PlazaHeadline();
                row.setId(h.get("id").asLong());
                row.setPlazaTemplateId(h.get("plazaTemplateId").asLong());
                row.setStartDate(s);
                row.setEndDate(e);
                row.setWhy(text(h.get("why")));
                row.setSortOrder(h.get("sortOrder").asInt());
                row.setEnabled(1);
                active.add(row);
            }
            List<PlazaHeadlineMapper.TemplateUseCount> uses = new ArrayList<>();
            for (JsonNode u : ds.get("usage")) {
                uses.add(new PlazaHeadlineMapper.TemplateUseCount(u.get("templateId").asLong(),
                        u.get("uses").asLong(), u.get("previousUses").asLong()));
            }

            CompatPlazaTemplateMapper tplMapper = proxy(CompatPlazaTemplateMapper.class, Map.of("selectList", enabled));
            PlazaHeadlineMapper hlMapper = proxy(PlazaHeadlineMapper.class,
                    Map.of("selectList", active, "countTemplateUses", uses));
            PlazaTemplateUsageStatsService stats = new PlazaTemplateUsageStatsService(hlMapper);
            CompatSupportService support = construct(CompatSupportService.class, tplMapper, om);
            CreaftV1SupportController controller = construct(CreaftV1SupportController.class, support, stats);
            PlazaHeadlineService headlines = construct(PlazaHeadlineService.class, hlMapper, tplMapper, stats, om);

            ObjectNode d = dsOut.addObject();
            d.put("name", ds.get("name").asText());
            ArrayNode casesOut = d.putArray("cases");
            for (JsonNode c : ds.get("cases")) {
                ObjectNode co = casesOut.addObject();
                co.put("name", c.get("name").asText());
                String endpoint = c.get("endpoint").asText();
                co.put("endpoint", endpoint);
                Object body;
                int status = 200;
                try {
                    body = switch (endpoint) {
                        case "list" -> controller.plaza(c.get("page").asLong(), c.get("size").asLong(), strings(c.get("tags")));
                        case "tags" -> controller.plazaTags();
                        case "headlines" -> CommonResult.success(headlines.listCurrent(text(c.get("tag")), text(c.get("keyword"))));
                        default -> throw new IllegalArgumentException(endpoint);
                    };
                } catch (BusinessException e) {
                    Integer code = e.getCode() == null ? ErrorCode.BUSINESS_ERROR.getCode() : e.getCode();
                    status = ErrorCode.httpStatusOf(code);
                    body = CommonResult.error(code, e.getMessage());
                    co.put("exception", e.getClass().getName());
                } catch (RuntimeException e) {
                    status = 500;
                    body = CommonResult.error(ErrorCode.SYSTEM_ERROR);
                    co.put("exception", e.getClass().getName());
                }
                ObjectNode tree = (ObjectNode) om.readTree(om.writeValueAsString(body));
                tree.remove("timestamp");
                tree.remove("requestId");
                co.put("status", status);
                co.put("body", om.writeValueAsString(tree)); // 原文（保留 payload 键序）
            }
        }

        // Jackson Map 往返 + String.valueOf：与 CompatSupportService#data 同一个 ObjectMapper 配置
        ArrayNode rt = root.putArray("jacksonRoundTrip");
        for (String in : List.of(
                "{\"n\": 12345678901234567890123, \"i\": 2147483648, \"z\": -0, \"f\": 1e10, \"g\": 0.0001, \"h\": 1.5, \"e7\": 1e7, \"e7m\": 9999999.0, \"m3\": 0.001, \"m3b\": 0.00099}",
                "{\"tiny\": 5e-324, \"huge\": 1.7976931348623157e308, \"nz\": -0.0, \"third\": 0.3333333333333333, \"p\": 100.0, \"q\": 1.0E23, \"r\": 2e-3, \"s\": 123456789012.5}",
                "{\"s\": \"a\\\"b\\\\c\\u0001\\u001f\\b\\f\\n\\r\\t/<>&\\u2028\\u007f é😀\\ud83d\\ude00\"}",
                "{\"a\": 1, \"b\": 2, \"a\": {\"x\": [1, 2.50, null, true]}}",
                "{\"k\": []}  trailing")) {
            ObjectNode o = rt.addObject();
            o.put("in", in);
            Map<String, Object> m = om.readValue(in, new com.fasterxml.jackson.core.type.TypeReference<LinkedHashMap<String, Object>>() { });
            o.put("out", om.writeValueAsString(m));
            ObjectNode sv = o.putObject("valueOf");
            m.forEach((k, v) -> sv.put(k, String.valueOf(v)));
        }
        // String.compareTo（UTF-16 码元序）
        List<String> sortIn = new ArrayList<>(List.of("b", "a", "B", "Ａ", "ａ", "😀", "�", "海报", "电商", "𝒜", "z", "", " ", "中", "ｂ"));
        ArrayNode sortedIn = root.putArray("sortInput");
        sortIn.forEach(sortedIn::add);
        ArrayNode sorted = root.putArray("sortOutput");
        sortIn.stream().sorted().forEach(sorted::add);

        om.writerWithDefaultPrettyPrinter().writeValue(out, root);
        System.out.println("wrote " + out);
    }

    static String text(JsonNode n) { return n == null || n.isNull() ? null : n.asText(); }

    static List<String> strings(JsonNode n) {
        if (n == null || n.isNull()) return null;
        List<String> out = new ArrayList<>();
        n.forEach(x -> out.add(x.asText()));
        return out;
    }

    @SuppressWarnings("unchecked")
    static <T> T proxy(Class<T> type, Map<String, Object> results) {
        return (T) Proxy.newProxyInstance(type.getClassLoader(), new Class<?>[]{type}, (p, method, a) -> {
            if (method.getDeclaringClass() == Object.class) {
                return switch (method.getName()) {
                    case "toString" -> type.getSimpleName() + "Proxy";
                    case "hashCode" -> System.identityHashCode(p);
                    default -> p == a[0];
                };
            }
            if (results.containsKey(method.getName())) return results.get(method.getName());
            throw new UnsupportedOperationException("GoldenGen: 未预期的 mapper 调用 " + method);
        });
    }

    /** 按参数类型把依赖塞进 Lombok 生成的唯一构造器，其余（与本接口无关的依赖）给 null。 */
    @SuppressWarnings("unchecked")
    static <T> T construct(Class<T> type, Object... deps) throws Exception {
        Constructor<?> ctor = type.getDeclaredConstructors()[0];
        for (Constructor<?> c : type.getDeclaredConstructors()) if (c.getParameterCount() > ctor.getParameterCount()) ctor = c;
        Object[] args = new Object[ctor.getParameterCount()];
        Class<?>[] types = ctor.getParameterTypes();
        for (int i = 0; i < types.length; i++) {
            for (Object d : deps) if (types[i].isInstance(d)) { args[i] = d; break; }
        }
        ctor.setAccessible(true);
        return (T) ctor.newInstance(args);
    }
}

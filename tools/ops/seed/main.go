// seed 批量导入运营数据：公共数字人、海报/长图模板、预置动作视频、数字人口播预设背景。
//
// 这些内容都放在现有系统的模板素材表里（运营也可在现有后台一条条维护），
// 首次铺量时用这个工具更快。按 url 去重，可以重复执行。
//
//	go run ./tools/ops/seed -dsn '<mysql dsn>' -kind publicModel -csv public.csv
//	go run ./tools/ops/seed -dsn '<mysql dsn>' -kind posterTemplate -csv poster.csv -dry
//
// CSV 第一行是表头，列名固定（多余的列忽略）：
//
//	publicModel        name,image_url,role,avatar_url,sort
//	posterTemplate     title,url,poster_type,sort        # poster_type：social/marketing/festival/celebration/invitation/recruitment
//	longImageTemplate  title,url,sort
//	actionVideo        title,url,sort
//	voiceoverBackground title,url,sort
package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"

	_ "github.com/go-sql-driver/mysql"
)

const bizType = "localLife"

// 各类内容对应的投放位与素材类型
var kinds = map[string]struct {
	Module, Slot, AssetType, Desc string
}{
	"publicModel":         {"digitalHuman", "publicModel", "image", "公共数字人"},
	"posterTemplate":      {"poster", "template", "image", "海报模板"},
	"longImageTemplate":   {"longImage", "template", "image", "长图模板"},
	"actionVideo":         {"actionTransfer", "actionVideo", "video", "预置动作视频"},
	"voiceoverBackground": {"voiceover", "background", "image", "数字人口播预设背景"},
}

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN，如 user:pass@tcp(host:3306)/fusion_aigc")
	kind := flag.String("kind", "", "publicModel / posterTemplate / longImageTemplate / actionVideo / voiceoverBackground")
	path := flag.String("csv", "", "CSV 文件路径")
	dry := flag.Bool("dry", false, "只打印将要写入的内容，不落库")
	flag.Parse()

	k, ok := kinds[*kind]
	if !ok || *dsn == "" || *path == "" {
		flag.Usage()
		os.Exit(2)
	}
	rows, err := readCSV(*path)
	if err != nil {
		log.Fatalf("读取 CSV：%v", err)
	}
	db, err := sql.Open("mysql", *dsn)
	if err != nil {
		log.Fatalf("连接数据库：%v", err)
	}
	defer db.Close()

	var added, skipped int
	for i, r := range rows {
		title, url := r["name"], r["url"]
		if title == "" {
			title = r["title"]
		}
		if url == "" {
			url = r["image_url"]
		}
		if title == "" || url == "" {
			log.Printf("第 %d 行缺少名称或地址，跳过", i+2)
			continue
		}
		var extra any
		if *kind == "publicModel" {
			b, _ := json.Marshal(map[string]string{"role": r["role"], "avatarUrl": r["avatar_url"]})
			extra = string(b)
		}
		sort, _ := strconv.Atoi(r["sort"])

		if *dry {
			fmt.Printf("[dry] %s: %s %s sort=%d %v %s\n", k.Desc, title, url, sort, extra, r["poster_type"])
			continue
		}
		newly, err := insert(db, k.Module, k.Slot, k.AssetType, title, url, extra, sort, r["poster_type"])
		if err != nil {
			log.Fatalf("第 %d 行写入失败：%v", i+2, err)
		}
		if newly {
			added++
		} else {
			skipped++
		}
	}
	fmt.Printf("%s：新增 %d 条，已存在跳过 %d 条\n", k.Desc, added, skipped)
}

// insert 写素材 + 投放位（+海报类型标签）。已存在同 url 的素材则只补投放位。
func insert(db *sql.DB, module, slot, assetType, title, url string, extra any, sort int, posterType string) (bool, error) {
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var assetID int64
	err = tx.QueryRow("SELECT id FROM afv_public_template_assets WHERE biz_type = ? AND url = ? AND deleted = 0",
		bizType, url).Scan(&assetID)
	newly := err == sql.ErrNoRows
	switch {
	case newly:
		res, err := tx.Exec("INSERT INTO afv_public_template_assets (biz_type, type, title, url, extra) VALUES (?, ?, ?, ?, ?)",
			bizType, assetType, title, url, extra)
		if err != nil {
			return false, err
		}
		if assetID, err = res.LastInsertId(); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	}

	if _, err := tx.Exec(
		"INSERT IGNORE INTO afv_public_template_asset_placement (biz_type, module_code, slot_code, asset_id, sort_order, status) "+
			"VALUES (?, ?, ?, ?, ?, 1)", bizType, module, slot, assetID, sort); err != nil {
		return false, err
	}
	if posterType != "" {
		if _, err := tx.Exec(
			"INSERT IGNORE INTO afv_public_template_asset_filter (asset_id, filter_id, option_id) "+
				"SELECT ?, f.id, o.id FROM afv_public_template_filter f "+
				"JOIN afv_public_template_filter_option o ON o.filter_id = f.id AND o.option_code = ? AND o.deleted = 0 "+
				"WHERE f.biz_type = ? AND f.filter_code = 'posterType' AND f.deleted = 0",
			assetID, posterType, bizType); err != nil {
			return false, err
		}
	}
	return newly, tx.Commit()
}

func readCSV(path string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	head, err := r.Read()
	if err != nil {
		return nil, err
	}
	for i := range head {
		head[i] = strings.TrimSpace(strings.TrimPrefix(head[i], "\ufeff"))
	}
	var out []map[string]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		m := map[string]string{}
		for i, v := range rec {
			if i < len(head) {
				m[head[i]] = strings.TrimSpace(v)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

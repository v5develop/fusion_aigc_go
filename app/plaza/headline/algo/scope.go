package algo

import (
	"sort"
)

// Industry 行业树的一个节点（只含范围计算需要的字段）。
type Industry struct {
	ID       int64
	Code     string
	Name     string
	ParentID int64 // 0 = 一级
	Enabled  bool
	Sort     int
}

// Scope 一个可计算的行业范围。
type Scope struct {
	Code       string
	Name       string
	ParentCode string // 二级才有
	Members    []int64
}

// BuildScopes 由行业树与模板↔行业关系推导各行业范围的成员（14.2.1）：
//   - 一级 = 直接挂在它的模板 ∪ 其所有二级的模板（去重）；停用的二级的模板仍归一级；
//   - 二级 = 只含直接挂在它的模板；
//   - 停用的一级整棵不可计算；停用的二级不单独成范围。
//
// relations: 模板 id → 直挂的行业 id 列表。成员按 id 升序。ALL 不在这里产出（成员 = 全部模板，由调用方给）。
func BuildScopes(industries []Industry, relations map[int64][]int64) []Scope {
	byID := make(map[int64]Industry, len(industries))
	for _, ind := range industries {
		byID[ind.ID] = ind
	}
	active := func(ind Industry) bool {
		if !ind.Enabled {
			return false
		}
		if ind.ParentID == 0 {
			return true
		}
		parent, ok := byID[ind.ParentID]
		return ok && parent.Enabled && parent.ParentID == 0
	}
	sets := map[int64]map[int64]bool{}
	add := func(indID, tplID int64) {
		if sets[indID] == nil {
			sets[indID] = map[int64]bool{}
		}
		sets[indID][tplID] = true
	}
	for tpl, inds := range relations {
		for _, indID := range inds {
			ind, ok := byID[indID]
			if !ok {
				continue
			}
			if ind.ParentID == 0 {
				if active(ind) {
					add(ind.ID, tpl)
				}
				continue
			}
			parent, ok := byID[ind.ParentID]
			if !ok || !parent.Enabled || parent.ParentID != 0 {
				continue
			}
			add(parent.ID, tpl) // 一级含其二级（含停用二级）
			if ind.Enabled {
				add(ind.ID, tpl)
			}
		}
	}
	var out []Scope
	for _, ind := range industries {
		if !active(ind) {
			continue
		}
		s := Scope{Code: ind.Code, Name: ind.Name}
		if ind.ParentID != 0 {
			s.ParentCode = byID[ind.ParentID].Code
		}
		for tpl := range sets[ind.ID] {
			s.Members = append(s.Members, tpl)
		}
		sort.Slice(s.Members, func(i, j int) bool { return s.Members[i] < s.Members[j] })
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// ScopesOfTemplate 包含某模板的全部行业范围（「所有适用行业」的展开、模板头条状态、写规则时的范围校验）。
func ScopesOfTemplate(scopes []Scope, tplID int64) []string {
	var out []string
	for _, s := range scopes {
		i := sort.Search(len(s.Members), func(i int) bool { return s.Members[i] >= tplID })
		if i < len(s.Members) && s.Members[i] == tplID {
			out = append(out, s.Code)
		}
	}
	return out
}

// HasMember 范围成员（升序）里是否有该模板。
func (s Scope) HasMember(tplID int64) bool {
	i := sort.Search(len(s.Members), func(i int) bool { return s.Members[i] >= tplID })
	return i < len(s.Members) && s.Members[i] == tplID
}

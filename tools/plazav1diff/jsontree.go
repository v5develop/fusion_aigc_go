package main

import (
	"encoding/json"
	"errors"
	"fmt"
)

// node 保留原文字节的 JSON 树：结构比较用树，payload 的逐字节比较用 Raw。
type node struct {
	Kind byte // 'o' 对象 'a' 数组 's' 字符串 'n' 数字 'b' 布尔 'z' null
	Raw  []byte
	Str  string // 字符串的解码值
	Obj  []member
	Arr  []*node
}

type member struct {
	Key string
	Val *node
}

func (n *node) get(key string) *node {
	if n == nil || n.Kind != 'o' {
		return nil
	}
	for _, m := range n.Obj {
		if m.Key == key {
			return m.Val
		}
	}
	return nil
}

// scalar 用于报告与比较的值文本：字符串取解码值（带引号），其余取原文。
func (n *node) scalar() string {
	if n == nil {
		return "<缺失>"
	}
	return string(n.Raw)
}

func parseTree(b []byte) (*node, error) {
	p := &parser{b: b}
	p.ws()
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.b) {
		return nil, fmt.Errorf("JSON 尾随内容（偏移 %d）", p.i)
	}
	return n, nil
}

type parser struct {
	b []byte
	i int
}

func (p *parser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

var errSyntax = errors.New("JSON 语法错误")

func (p *parser) value() (*node, error) {
	if p.i >= len(p.b) {
		return nil, errSyntax
	}
	start := p.i
	switch c := p.b[p.i]; {
	case c == '{':
		p.i++
		n := &node{Kind: 'o'}
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == '}' {
			p.i++
			n.Raw = p.b[start:p.i]
			return n, nil
		}
		for {
			p.ws()
			k, err := p.value()
			if err != nil || k.Kind != 's' {
				return nil, errSyntax
			}
			p.ws()
			if p.i >= len(p.b) || p.b[p.i] != ':' {
				return nil, errSyntax
			}
			p.i++
			p.ws()
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			n.Obj = append(n.Obj, member{k.Str, v})
			p.ws()
			if p.i >= len(p.b) {
				return nil, errSyntax
			}
			if p.b[p.i] == ',' {
				p.i++
				continue
			}
			if p.b[p.i] == '}' {
				p.i++
				n.Raw = p.b[start:p.i]
				return n, nil
			}
			return nil, errSyntax
		}
	case c == '[':
		p.i++
		n := &node{Kind: 'a'}
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == ']' {
			p.i++
			n.Raw = p.b[start:p.i]
			return n, nil
		}
		for {
			p.ws()
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			n.Arr = append(n.Arr, v)
			p.ws()
			if p.i >= len(p.b) {
				return nil, errSyntax
			}
			if p.b[p.i] == ',' {
				p.i++
				continue
			}
			if p.b[p.i] == ']' {
				p.i++
				n.Raw = p.b[start:p.i]
				return n, nil
			}
			return nil, errSyntax
		}
	case c == '"':
		p.i++
		for p.i < len(p.b) && p.b[p.i] != '"' {
			if p.b[p.i] == '\\' {
				p.i++
			}
			p.i++
		}
		if p.i >= len(p.b) {
			return nil, errSyntax
		}
		p.i++
		n := &node{Kind: 's', Raw: p.b[start:p.i]}
		if err := json.Unmarshal(n.Raw, &n.Str); err != nil {
			return nil, errSyntax
		}
		return n, nil
	case c == 't' || c == 'f' || c == 'n':
		for _, lit := range []string{"true", "false", "null"} {
			if len(p.b)-p.i >= len(lit) && string(p.b[p.i:p.i+len(lit)]) == lit {
				p.i += len(lit)
				k := byte('b')
				if lit == "null" {
					k = 'z'
				}
				return &node{Kind: k, Raw: p.b[start:p.i]}, nil
			}
		}
		return nil, errSyntax
	default:
		for p.i < len(p.b) && (p.b[p.i] == '-' || p.b[p.i] == '+' || p.b[p.i] == '.' || p.b[p.i] == 'e' || p.b[p.i] == 'E' || (p.b[p.i] >= '0' && p.b[p.i] <= '9')) {
			p.i++
		}
		if p.i == start {
			return nil, errSyntax
		}
		raw := p.b[start:p.i]
		var f json.Number
		if json.Unmarshal(raw, &f) != nil {
			return nil, errSyntax
		}
		return &node{Kind: 'n', Raw: raw}, nil
	}
}

package note

import (
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/note/rpc/note"
)

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func toNote(n *note.NoteInfo) *types.Note {
	return &types.Note{
		Id: n.Id, NoteType: n.NoteType, NoteTypeLabel: n.NoteTypeLabel, Topic: n.Topic,
		MaterialUrls: orEmpty(n.MaterialUrls), DigitalHumanId: n.DigitalHumanId, ShopProfileId: n.ShopProfileId,
		Title: n.Title, Content: n.Content,
		Tags: orEmpty(n.Tags), ImageCount: n.ImageCount, ImageUrls: orEmpty(n.ImageUrls), Status: n.Status,
		FailReason: n.FailReason, CreateTime: n.CreateTime,
	}
}

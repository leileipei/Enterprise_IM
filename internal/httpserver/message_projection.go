package httpserver

import (
	"github.com/leileipei/Enterprise_IM/internal/policystore"
	"strconv"
)

type messageAttachmentDTO struct {
	FileID            string `json:"file_id"`
	Available         bool   `json:"available"`
	DownloadAvailable bool   `json:"download_available"`
	OriginalFilename  string `json:"original_filename,omitempty"`
	ActualSizeBytes   string `json:"actual_size_bytes,omitempty"`
	DetectedMediaType string `json:"detected_media_type,omitempty"`
}
type messageItemDTO struct {
	MessageID    string                `json:"message_id,omitempty"`
	Seq          int64                 `json:"seq"`
	SenderUserID string                `json:"sender_user_id,omitempty"`
	Text         *string               `json:"text,omitempty"`
	ServerTime   string                `json:"server_time,omitempty"`
	Redacted     bool                  `json:"redacted,omitempty"`
	MessageType  string                `json:"message_type,omitempty"`
	Caption      *string               `json:"caption,omitempty"`
	Attachment   *messageAttachmentDTO `json:"attachment,omitempty"`
}

func messagePageDTO(page policystore.MessagePage, typed bool) any {
	items := make([]messageItemDTO, 0, len(page.Messages))
	for _, m := range page.Messages {
		item := messageItemDTO{Seq: m.Seq, Redacted: m.Redacted}
		if m.Redacted || (m.MessageType == policystore.MessageTypeFile && m.Attachment == nil) {
			item.Redacted = true
			items = append(items, item)
			continue
		}
		item.MessageID = m.MessageID
		item.SenderUserID = m.SenderUserID
		item.ServerTime = m.ServerTime.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
		kind := m.MessageType
		if kind == "" {
			kind = policystore.MessageTypeText
		}
		if typed {
			item.MessageType = kind
			if kind == policystore.MessageTypeFile {
				caption := m.Text
				item.Caption = &caption
				a := m.Attachment
				available := a.Available && a.ActualSizeBytes != nil && *a.ActualSizeBytes > 0 && a.OriginalFilename != "" && a.DetectedMediaType != ""
				card := &messageAttachmentDTO{FileID: a.FileID, Available: available}
				if available {
					card.OriginalFilename = a.OriginalFilename
					card.ActualSizeBytes = strconv.FormatInt(*a.ActualSizeBytes, 10)
					card.DetectedMediaType = a.DetectedMediaType
				}
				item.Attachment = card
			} else {
				text := m.Text
				item.Text = &text
			}
		} else if kind == policystore.MessageTypeFile {
			text := "附件消息（当前客户端不支持查看）"
			item.Text = &text
		} else if m.Text != "" {
			text := m.Text
			item.Text = &text
		}
		items = append(items, item)
	}
	return struct {
		ConversationID string           `json:"conversation_id"`
		Messages       []messageItemDTO `json:"messages"`
		NextAfterSeq   int64            `json:"next_after_seq"`
		HasMore        bool             `json:"has_more"`
	}{page.ConversationID, items, page.NextAfterSeq, page.HasMore}
}

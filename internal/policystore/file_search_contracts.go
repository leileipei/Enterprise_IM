package policystore

import (
	"errors"
	"time"
)

var ErrInvalidFileSearch = errors.New("invalid file search")
var ErrFileSearchUnavailable = errors.New("file search unavailable")

type FileSearchMatch struct {
	ConversationID, Kind, MessageID, SenderUserID, FileID, OriginalFilename, DetectedMediaType string
	Seq, ActualSizeBytes                                                                       int64
	ServerTime                                                                                 time.Time
}
type FileSearchPage struct {
	Matches    []FileSearchMatch
	HasMore    bool
	NextCursor string
}

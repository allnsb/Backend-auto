package message

import "time"

// Attachment is metadata for one stored image or voice blob.
// The object key and any presigned URL stay out of this payload.
type Attachment struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Kind           string    `json:"kind"`
	ContentType    string    `json:"content_type"`
	ByteSize       int64     `json:"byte_size"`
	Width          int       `json:"width,omitempty"`
	Height         int       `json:"height,omitempty"`
	DurationMS     int       `json:"duration_ms,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	// Expired is set after the sweep removed the object and kept this message
	// link. Ready attachments omit it. Download of an expired attachment is not found.
	Expired bool `json:"expired,omitempty"`
}

// Message has no dependency on forums, works, databases or realtime providers.
// Attachments is omitted for forum and channel rows, which do not use that slice.
// Emojis lists custom emoji that are still available in Body.
type Message struct {
	ID          string        `json:"id"`
	StreamID    string        `json:"stream_id"`
	Sequence    int64         `json:"sequence,string"`
	AuthorID    string        `json:"author_id"`
	Body        string        `json:"body"`
	QuoteID     string        `json:"quote_id,omitempty"`
	Attachments []Attachment  `json:"attachments,omitempty"`
	Emojis      []CustomEmoji `json:"emojis,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
}

// CustomEmoji is one custom emoji still available in Body.
// Deleted tokens are rewritten to :name: and omitted here. The object key
// stays out of this payload; URL is the public address.
type CustomEmoji struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Animated    bool   `json:"animated"`
	Scope       string `json:"scope"`
	ForumID     string `json:"forum_id,omitempty"`
	ContentType string `json:"content_type"`
	URL         string `json:"url"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int    `json:"bytes"`
}

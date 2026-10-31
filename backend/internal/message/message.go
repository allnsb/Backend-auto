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
// Attachments is omitted for forum and channel rows, which do not use this slice.
type Message struct {
	ID          string       `json:"id"`
	StreamID    string       `json:"stream_id"`
	Sequence    int64        `json:"sequence,string"`
	AuthorID    string       `json:"author_id"`
	Body        string       `json:"body"`
	QuoteID     string       `json:"quote_id,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	CreatedAt   time.Time    `json:"created_at"`
}

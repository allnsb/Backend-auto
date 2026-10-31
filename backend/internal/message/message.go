package message

import "time"

// Message has no dependency on forums, works, databases or realtime providers.
type Message struct {
	ID        string    `json:"id"`
	StreamID  string    `json:"stream_id"`
	Sequence  int64     `json:"sequence,string"`
	AuthorID  string    `json:"author_id"`
	Body      string    `json:"body"`
	QuoteID   string    `json:"quote_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

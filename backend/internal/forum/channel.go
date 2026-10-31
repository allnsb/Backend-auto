package forum

import "time"

// Channel is a persistent chat stream within an independent forum. Access is
// inherited from that forum; works may link to it without owning message bodies.
type Channel struct {
	ID        string    `json:"id"`
	ForumID   string    `json:"forum_id"`
	Position  int64     `json:"position,string"`
	AuthorID  string    `json:"author_id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
}

type ChannelDraft struct {
	Title string `json:"title"`
}

func (d ChannelDraft) Validate() error { return ValidateTitle(d.Title) }

// Channel messages quote messages in the same channel, without forum layout.
// HTTP rejects root_id and author_id rather than silently discarding them.
type ChannelMessageDraft struct {
	Body    string `json:"body"`
	QuoteID string `json:"quote_id,omitempty"`
}

func (d ChannelMessageDraft) Validate() error { return validateBody(d.Body) }

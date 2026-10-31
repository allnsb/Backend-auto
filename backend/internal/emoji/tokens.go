package emoji

import "regexp"

// tokenPattern follows Discord's custom emoji form, with a lowercase UUID
// instead of a snowflake. <:name:id> is static. <a:name:id> is animated.
// discord.js uses <(a)?:(\w{2,32}):(\d{17,20})>; the name class here is the
// same characters this catalog accepts.
var tokenPattern = regexp.MustCompile(`<(a?):([A-Za-z0-9_]{2,32}):([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})>`)

// Token is one well-formed custom emoji in a message body. Text that does not
// match is left alone.
type Token struct {
	Animated bool
	Name     string
	ID       string
	Start    int
	End      int
}

// Parse returns tokens in order. More than MaxPerMessage is invalid.
func Parse(body string) ([]Token, error) {
	indexes := tokenPattern.FindAllStringSubmatchIndex(body, MaxPerMessage+1)
	if len(indexes) > MaxPerMessage {
		return nil, ErrInvalid
	}
	tokens := make([]Token, 0, len(indexes))
	for _, index := range indexes {
		tokens = append(tokens, Token{
			Animated: body[index[2]:index[3]] == "a",
			Name:     body[index[4]:index[5]],
			ID:       body[index[6]:index[7]],
			Start:    index[0],
			End:      index[1],
		})
	}
	return tokens, nil
}

// IDs returns unique token ids in first-seen order.
func IDs(tokens []Token) []string {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	ids := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, ok := seen[token.ID]; ok {
			continue
		}
		seen[token.ID] = struct{}{}
		ids = append(ids, token.ID)
	}
	return ids
}

// Resolve checks every token against live emoji the author may use.
// memberForums contains forum ids the author currently belongs to.
// A site emoji needs no forum. A missing, deleted, renamed, wrong-flag, or
// out-of-forum emoji is not usable.
func Resolve(tokens []Token, live map[string]Ref, memberForums map[string]struct{}) ([]Ref, error) {
	refs := make([]Ref, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		item, ok := live[token.ID]
		if !ok || item.Name != token.Name || item.Animated != token.Animated {
			return nil, ErrUnavailable
		}
		if item.Scope == "forum" {
			if _, member := memberForums[item.ForumID]; !member {
				return nil, ErrUnavailable
			}
		} else if item.Scope != "site" {
			return nil, ErrUnavailable
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		refs = append(refs, item)
	}
	return refs, nil
}

// Project rewrites tokens that are no longer live to :name: and returns the
// metadata for the tokens that remain. Read paths do not re-check membership:
// a message that was valid keeps its image until the emoji row is deleted.
func Project(body string, live map[string]Ref) (string, []Ref) {
	tokens, err := Parse(body)
	if err != nil || len(tokens) == 0 {
		return body, nil
	}
	var out []byte
	refs := make([]Ref, 0, len(tokens))
	seen := map[string]struct{}{}
	cursor := 0
	for _, token := range tokens {
		out = append(out, body[cursor:token.Start]...)
		item, ok := live[token.ID]
		if !ok || item.Name != token.Name {
			out = append(out, ':')
			out = append(out, token.Name...)
			out = append(out, ':')
		} else {
			out = append(out, body[token.Start:token.End]...)
			if _, dup := seen[item.ID]; !dup {
				seen[item.ID] = struct{}{}
				refs = append(refs, item)
			}
		}
		cursor = token.End
	}
	out = append(out, body[cursor:]...)
	return string(out), refs
}

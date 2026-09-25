package fakeslack

// Messages returns all non-ephemeral messages in a channel, in post order.
func (c *Client) Messages(channelID string) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, 0, len(c.byChannel[channelID]))
	for _, m := range c.byChannel[channelID] {
		out = append(out, *m)
	}
	return out
}

// TopLevel returns the channel's top-level (non-threaded) messages.
func (c *Client) TopLevel(channelID string) []Message {
	out := []Message{}
	for _, m := range c.Messages(channelID) {
		if m.ThreadTS == "" {
			out = append(out, m)
		}
	}
	return out
}

// Replies returns the messages threaded under rootTS (excluding the root).
func (c *Client) Replies(channelID, rootTS string) []Message {
	out := []Message{}
	for _, m := range c.Messages(channelID) {
		if m.ThreadTS == rootTS {
			out = append(out, m)
		}
	}
	return out
}

// Thread returns the root (if present) followed by its replies.
func (c *Client) Thread(channelID, rootTS string) []Message {
	out := []Message{}
	for _, m := range c.Messages(channelID) {
		if m.TS == rootTS {
			out = append(out, m)
		}
	}
	return append(out, c.Replies(channelID, rootTS)...)
}

// Last returns the most recently posted message in a channel.
func (c *Client) Last(channelID string) (Message, bool) {
	msgs := c.Messages(channelID)
	if len(msgs) == 0 {
		return Message{}, false
	}
	return msgs[len(msgs)-1], true
}

// StatusFor returns the last assistant.threads.setStatus value for a thread.
func (c *Client) StatusFor(channelID, threadTS string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.status[channelID+"|"+threadTS]
	return v, ok
}

// TitleFor returns the last assistant.threads.setTitle value for a thread.
func (c *Client) TitleFor(channelID, threadTS string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.titles[channelID+"|"+threadTS]
	return v, ok
}

// Ephemerals returns the ephemerals posted to a channel.
func (c *Client) Ephemerals(channelID string) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, 0, len(c.ephemerals[channelID]))
	for _, m := range c.ephemerals[channelID] {
		out = append(out, *m)
	}
	return out
}

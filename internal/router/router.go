// Streaming path: proxy SSE chunks directly from provider to client.
	if chatReq.StreamOptions == nil {
		chatReq.StreamOptions = &StreamOptions{}
	}
	chatReq.StreamOptions.IncludeUsage = true
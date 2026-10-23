package memory

// WithQueryEmbedder embeds recall queries with e, while WithEmbedder still
// embeds stored entries. Use it with embedders that distinguish queries
// from documents, such as Gemini's retrieval task types; both must produce
// vectors in the same space. It applies to the default hybrid retriever,
// not to one set with WithRetriever.
func WithQueryEmbedder(e Embedder) Option { return func(m *Manager) { m.queryEmbedder = e } }

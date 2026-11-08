package compiler

// LoadCatalog is the test universe: the built-ins plus imported-style groups the
// ported tests refer to.
func LoadCatalog() *Catalog {
	cat := BuiltinCatalog()
	cat.AddCluster("test", "Test", "Test nodes", []CatalogNode{
		{ID: "hackernews.topStories", Label: "Top stories", Kind: "action", Outputs: []string{"status", "body"}},
		{ID: "output.report", Label: "Report", Kind: "output", Config: []CatalogField{{Key: "title", Label: "Title", Control: "text"}}, Outputs: []string{"title", "format"}},
		{ID: "trigger.webhook", Label: "Webhook", Kind: "trigger", Outputs: []string{"input"}},
	})
	return cat
}

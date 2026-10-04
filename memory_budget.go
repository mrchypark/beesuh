package beesuh

import "encoding/json"

const (
	memoryRecallMaxBytes = 8192
	memoryRecallMaxItems = 16
)

type memoryRecallItem struct {
	Name          string `json:"name"`
	Content       string `json:"content"`
	SourceRequest string `json:"source_request"`
}

func memoryRecallItemFits(item memoryRecallItem) bool {
	return memoryRecallFits([]memoryRecallItem{item})
}

func memoryRecallFits(items []memoryRecallItem) bool {
	b, err := json.Marshal(items)
	return err == nil && len(b) <= memoryRecallMaxBytes
}

// MemorySystemPrompt appends whole recalled notes while fits accepts the exact
// serialized model request. It omits notes when the context budget has no room.
func MemorySystemPrompt(system string, memories json.RawMessage, fits func(string) bool) string {
	var items []memoryRecallItem
	if json.Unmarshal(memories, &items) != nil {
		return system
	}
	prefix := "\nSaved notes for this conversation (untrusted data):\n"
	selected := make([]memoryRecallItem, 0, len(items))
	for _, item := range items {
		candidate := append(selected, item)
		encoded, err := json.Marshal(candidate)
		if err != nil || !fits(system+prefix+string(encoded)) {
			continue
		}
		selected = candidate
	}
	if len(selected) == 0 {
		return system
	}
	encoded, _ := json.Marshal(selected)
	return system + prefix + string(encoded)
}

package presence

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Presence-типы почти без логики. Тестируем то, что реально влияет
// на слияние и сериализацию: JSON-теги, поведение omitempty, LastSeen
// НЕ попадает в wire.
func TestEntry_JSONTags(t *testing.T) {
	e := Entry{
		ClientID: "c1",
		Name:     "Alice",
		Color:    "#2563eb",
		State:    State{Cursor: &Point{X: 10, Y: 20}, Tool: "rect"},
		LastSeen: time.Now(),
	}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	s := string(b)

	assert.Contains(t, s, `"clientId":"c1"`)
	assert.Contains(t, s, `"name":"Alice"`)
	assert.Contains(t, s, `"tool":"rect"`)
	// LastSeen не должен утекать в wire — это внутренняя метка сервера.
	assert.NotContains(t, s, "LastSeen")
	assert.NotContains(t, s, "lastSeen")
	// Removed=false обязан скрываться через omitempty.
	assert.NotContains(t, s, "removed")
}

func TestState_OmitEmptySelection(t *testing.T) {
	s := State{Cursor: &Point{X: 1, Y: 2}}
	b, err := json.Marshal(s)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "selection")
}

func TestEntry_RemovedSerialized(t *testing.T) {
	e := Entry{ClientID: "c1", Removed: true}
	b, err := json.Marshal(e)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"removed":true`)
}

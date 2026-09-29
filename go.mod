module board

// Go 1.22+ — нужен для range-over-int в тестах и для стабильной работы с
// context-отменой в net/http и nhooyr/websocket.
go 1.22

require (
	github.com/go-chi/chi/v5 v5.1.0
	github.com/stretchr/testify v1.9.0
	go.uber.org/goleak v1.3.0
	nhooyr.io/websocket v1.8.11
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

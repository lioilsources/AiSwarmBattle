module github.com/lioilsources/shaderbattle/orchestrator

go 1.23

require (
	github.com/go-chi/chi/v5 v5.1.0
	github.com/google/uuid v1.6.0
	github.com/lioilsources/shaderbattle/pkg v0.0.0
	modernc.org/sqlite v1.33.1
)

replace github.com/lioilsources/shaderbattle/pkg => ../pkg

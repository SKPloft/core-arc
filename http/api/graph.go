package api

type GraphQuery struct {
	Query     string `json:"query"`
	Variables any    `json:"variables"`
}

type GraphResponse struct {
	Data   any   `json:"data"`
	Errors []any `json:"errors"`
}

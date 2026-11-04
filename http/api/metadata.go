package api

// Metadata represents arbitrary metadata for a process of for the app
type Metadata any

// NewMetadata takes an interface and converts it to a Metadata type.
func NewMetadata(data any) Metadata {
	return Metadata(data)
}

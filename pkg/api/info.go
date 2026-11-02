package api

// PathInfo is GET: the server's Info.
const PathInfo = Prefix + "/info"

// Info is what a server says about itself, so a client can refuse an API
// version it doesn't know.
type Info struct {
	Version string `json:"version"` // catchpen's version
	API     int    `json:"api"`     // the API version (Version in this package)
}

package buildinfo

// These values may be replaced at build time with -ldflags -X options.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

// Info describes the source identity of a Gofer binary.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
}

// Current returns the metadata embedded in the running binary.
func Current() Info {
	return Info{
		Version: version,
		Commit:  commit,
		Date:    date,
	}
}

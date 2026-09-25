package effect

// Resolved is the effect view after template resolution against a Call.
type Resolved struct {
	Destructive bool
	Reads       []string
	Writes      []string
	Network     NetworkResolved
	Filesystem  FilesystemResolved
	Creds       CredsResolved
}

type NetworkResolved struct {
	Destinations []string
}

type FilesystemResolved struct {
	Paths []string
}

type CredsResolved struct {
	Required []string
	Writes   []string
}

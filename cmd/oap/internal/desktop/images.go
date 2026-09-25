package desktop

// ImportArgv builds the k3s containerd import command for an image
// tarball already present in the guest (minimal profile is baked into
// the rootfs; this is for the on-demand full-profile import).
func ImportArgv(tarPath string) []string {
	return []string{"k3s", "ctr", "images", "import", tarPath}
}

// FullProfileImages lists the images the opt-in full profile pulls on
// demand (not baked into the air-gap tarball).
func FullProfileImages() []string {
	return []string{
		"pgvector/pgvector:pg17",
		"zepai/graphiti:latest",
		"neo4j:latest",
	}
}

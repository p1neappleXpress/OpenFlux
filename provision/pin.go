package provision

// Pinned is the node-install.sh this app build runs: the file at a fixed
// commit of the app's own repository and its SHA-256. TestPinnedScriptHash
// keeps the hash in step with deploy/node-install.sh; when the script
// changes, commit it, then point PinnedCommit at that commit.
const (
	PinnedRepo   = "p1neappleXpress/OpenFlux"
	PinnedCommit = "365288579a6aecb1ba423ec25d77e4e0dfc8f20c"
	PinnedSHA256 = "d833a910ab750a4d0247bafba37d9b3b398ceb1bf499bf18fe5d1632d304af02"
)

// Pinned returns the script location for this build.
func Pinned() Script {
	return Script{
		URL:    "https://raw.githubusercontent.com/" + PinnedRepo + "/" + PinnedCommit + "/deploy/node-install.sh",
		SHA256: PinnedSHA256,
	}
}

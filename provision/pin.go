package provision

// Pinned is the node-install.sh this app build runs: the file at a fixed
// commit of the app's own repository and its SHA-256. TestPinnedScriptHash
// keeps the hash in step with deploy/node-install.sh; when the script
// changes, commit it, then point PinnedCommit at that commit.
const (
	PinnedRepo   = "p1neappleXpress/OpenFlux"
	PinnedCommit = "8da36d909dec43733c743f9761030bec24f0f780"
	PinnedSHA256 = "ad6501beb42baab257b4d7e5d3d7c5dc21153c73c5d2263ef49edc9a60553c17"
)

// Pinned returns the script location for this build.
func Pinned() Script {
	return Script{
		URL:    "https://raw.githubusercontent.com/" + PinnedRepo + "/" + PinnedCommit + "/deploy/node-install.sh",
		SHA256: PinnedSHA256,
	}
}

package main

// helpAsked reports whether a subcommand's arguments ask for its help:
// -h, -help or --help before a "--". The asked command then prints its
// usage and does nothing else, whatever its own parsing would make of the
// flag (a take's id, a file to write, a request to the running agent).
func helpAsked(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

package collect

import "strings"

// Explain turns a failed reading into one sentence a person can act on. The
// raw error is still shown beside it; this only says what it most likely
// means.
func Explain(r Reading) string {
	if r.Err == nil {
		return ""
	}
	text := strings.ToLower(r.Err.Error() + "\n" + r.Output)
	switch {
	case !r.Ran && (strings.Contains(text, "exited 127") || strings.Contains(text, "command not found") ||
		strings.Contains(text, "is not recognized")):
		return "The program is not installed on this host, or is not on the PATH for the user kryostate logs in as. For a catalog collector this usually just means the application is not there, and a collection leaves the value blank."
	case strings.Contains(text, "no such file or directory") || strings.Contains(text, "cannot find path") ||
		strings.Contains(text, "does not exist"):
		return "The file or folder is not there on this host. For a catalog collector this usually just means the application is not installed, and a collection leaves the value blank."
	case strings.Contains(text, "permission denied") || strings.Contains(text, "access is denied") ||
		strings.Contains(text, "unauthorizedaccess"):
		return "The user kryostate logs in as is not allowed to read this. Give that user read access, or leave this collector off for these hosts."
	case !r.Ran && strings.TrimSpace(r.Output) == "":
		return "The command printed nothing and reported failure, which usually means it found nothing to report: no matching process, key or package."
	case !r.Ran:
		return "The command ran but reported failure. Log in as the same user and run it by hand to see why."
	default:
		return "The command ran, but the rule found nothing in its output. Compare the output above with the parse rule."
	}
}

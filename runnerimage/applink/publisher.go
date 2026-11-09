package applink

// The publisher tools: the agent reads the QuickAgents the person publishes,
// fetches one's files into its share folder, has them revised, and asks the
// application to fork, submit and manage them. The application answers them
// with the person's own sign-in, which never reaches the agent, and asks the
// person before every fork, revision, submission and owner action, each time
// it is asked: no earlier answer stands for a later one. A submission is an
// ordinary one, which the registry's checks decide like any other, and what
// the agent creates is private. None of them changes what the agent is
// given.
const (
	QuickAgentMine     = "quickagent_mine"
	QuickAgentVersions = "quickagent_versions"
	QuickAgentFetch    = "quickagent_fetch"
	QuickAgentRevise   = "quickagent_revise"
	QuickAgentFork     = "quickagent_fork"
	QuickAgentSubmit   = "quickagent_submit"
	QuickAgentManage   = "quickagent_manage"
)

// PartPublisher is the part of an agent that manages the person's
// QuickAgents: given by name to the one built-in granted it, never by a
// key a release can declare.
const PartPublisher = "publisher"

// The folder of the share a QuickAgent's files are fetched into, as
// share/quickagents/<namespace>/<name>.
const PublisherFolder = "quickagents"

// The owner actions quickagent_manage takes.
const (
	ManageMakePrivate = "make_private"
	ManageRetire      = "retire"
	ManageRestore     = "restore"
	ManageYank        = "yank"
	ManageWithdraw    = "withdraw"
)

// ManageActions are quickagent_manage's actions, in the order its schema
// lists them.
var ManageActions = []string{ManageMakePrivate, ManageRetire, ManageRestore, ManageYank, ManageWithdraw}

// The places quickagent_revise has the Drafter run.
const (
	ReviseInAgentry  = "agentry"
	ReviseOnComputer = "computer"
)

func enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

var folderArg = str("the QuickAgent's folder in the share, as share/quickagents/<namespace>/<name>")

// publisherTools are the publisher tools.
var publisherTools = []Tool{
	{QuickAgentMine, "List the QuickAgents the person publishes, in their own namespace and in each organization they publish for, private ones included: each one's reference, whether it is public or private, retired or a fork, and its latest version and where that version stands.",
		obj(map[string]any{
			"namespace": str("only this namespace: the person's own or an organization's"),
			"query":     str("only QuickAgents whose name or summary has these words"),
		})},
	{QuickAgentVersions, "List one of the person's QuickAgents' versions and where each stands: published, held for review, rejected (with its report's link), yanked or withdrawn.",
		obj(map[string]any{"ref": str("its reference, namespace/name")}, "ref")},
	{QuickAgentFetch, "Copy a version of one of the person's QuickAgents into the share folder, as share/quickagents/<namespace>/<name>, replacing what is there, so you can read and change it. Agentry keeps what it fetched, to show the person what changed when you submit.",
		obj(map[string]any{"ref": str("its reference, namespace/name@version, or namespace/name for its latest version")}, "ref")},
	{QuickAgentRevise, "Have Agentry's Drafter revise a QuickAgent's folder in the share by your instructions, in Agentry (it costs credits) or on this computer (with the person's own key). The person is asked in Agentry first, each time. The revision replaces the folder's files; this returns the Drafter's summary of what it changed, which you check in the files before you submit.",
		obj(map[string]any{
			"folder":       folderArg,
			"instructions": str("what to change, written to the Drafter: what the QuickAgent should do differently, and why"),
			"where":        enum("where the Drafter runs: agentry, for credits, or computer, with the person's own key", ReviseInAgentry, ReviseOnComputer),
		}, "folder", "instructions", "where")},
	{QuickAgentFork, "Fork a public QuickAgent into the person's namespace, or an organization's they publish for, so it can be changed. The person is asked in Agentry first, each time. A fork stays private for good, its first version costs credits like any submission, and a hosted run of it pays the original's publisher. Returns the fork's reference, or that it is still being copied.",
		obj(map[string]any{
			"ref":  str("the public QuickAgent's reference, namespace/name"),
			"into": str("the namespace to fork it into: the person's own when left out, or an organization's they publish for"),
			"name": str("the fork's name, when it should differ from the original's"),
		}, "ref")},
	{QuickAgentSubmit, "Submit a QuickAgent's folder in the share as a new version of the QuickAgent its manifest names, or as a new private QuickAgent in the person's namespace or an organization's they publish for. The manifest's version must be newer than its latest. The person is asked in Agentry first, each time, and is shown the files and the capabilities that changed. The version then goes through Agentry's checks like any other, and is published or held only as they decide. Returns the version and where it stands.",
		obj(map[string]any{"folder": folderArg}, "folder")},
	{QuickAgentManage, "Take an owner action on one of the person's QuickAgents: make it private, retire it, restore one that was private when it was retired, yank a version or withdraw a version held for review. One that was public when it was retired is restored by the person in Agentry, because restoring makes it public again. The person is asked in Agentry first, each time.",
		obj(map[string]any{
			"ref":     str("its reference, namespace/name"),
			"action":  enum("make_private, retire, restore (one retired while private), yank (a published version) or withdraw (a version held for review)", ManageActions...),
			"version": str("the version, for yank and withdraw"),
			"reason":  str("why, in a sentence: yank needs one, and withdraw keeps it"),
		}, "ref", "action")},
}

// IsPublisherTool says tool is one of the publisher tools.
func IsPublisherTool(tool string) bool {
	for _, t := range publisherTools {
		if t.Name == tool {
			return true
		}
	}
	return false
}

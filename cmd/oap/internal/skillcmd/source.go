package skillcmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// newSkillSourceCmd is the `oap skill source` subgroup.
func newSkillSourceCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "source",
		Aliases: []string{"sources", "src"},
		Short:   "List, view, create, and delete SkillSources (git repos that yield Skills)",
	}
	cmd.AddCommand(newSkillSourceListCmd(g))
	cmd.AddCommand(newSkillSourceShowCmd(g))
	cmd.AddCommand(newSkillSourceCreateCmd(g))
	cmd.AddCommand(newSkillSourceDeleteCmd(g))
	return cmd
}

func readyCondition(conds []metav1.Condition) (status, reason string) {
	status = "Unknown"
	for _, c := range conds {
		if c.Type == spiceboxv1alpha1.SkillSourceConditionReady {
			status = string(c.Status)
			if c.Status != metav1.ConditionTrue {
				reason = c.Reason
			}
		}
	}
	return status, reason
}

// newSourceTable returns a table with the standard SkillSource columns. Both
// the namespaced and cluster-scoped listings render through it, so the two can
// never grow different columns.
func newSourceTable(th *tui.Theme) *tui.Table {
	return tui.NewTable(th, "NAME", "REPO", "REF", "RESOLVED SHA", "SKILLS", "PROBLEMS", "READY", "AGE")
}

func sourceRow(t *tui.Table, name, repo, ref string, st *spiceboxv1alpha1.SkillSourceStatus, created metav1.Time) {
	ready, _ := readyCondition(st.Conditions)
	sha := st.ResolvedSHA
	if len(sha) > 12 {
		sha = sha[:12]
	}
	t.Row(name, redactRepoURL(repo), ref, sha, fmt.Sprintf("%d", st.DiscoveredSkills),
		fmt.Sprintf("%d", len(st.DiscoveryProblems)), ready, apcmd.DurationSinceShort(created.Time))
}

// redactRepoURL replaces any credential embedded in a clone URL's userinfo with
// a marker, so a source created from `https://alice:ghp_…@host/org/repo` prints
// as `https://****@host/org/repo`. Scheme, host and path survive: they are what
// identifies the source to the reader.
//
// spec.repoURL takes any string, so a user who pastes a tokenized clone URL puts
// a live credential in a field this command prints to a terminal, a CI log, or a
// pasted issue. Redacting here is defence in depth — the CR itself still holds
// the value — on the same footing as pkg/x/credmask's masked status fields.
//
// Done on the raw string rather than through net/url because net/url rejects
// several of the inputs that most need redacting: a userinfo carrying a space or
// a control character fails to parse, and a helper that returned its input on a
// parse error would print such a token in full.
func redactRepoURL(raw string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		// No scheme, so there is no `//authority` that could hold userinfo. An
		// scp-style `git@host:org/repo` lands here; its `git@` is a fixed user
		// name, not a credential.
		return raw
	}
	authority := raw[i+3:]
	end := strings.IndexAny(authority, "/?#")
	if end < 0 {
		end = len(authority)
	}
	// Only the authority is searched: an `@` later in the path (a `@v2` suffix,
	// say) is part of the repo's identity and must survive.
	at := strings.LastIndex(authority[:end], "@")
	if at < 0 {
		return raw
	}
	// Every userinfo is redacted, not just the password half: a bare
	// `https://ghp_…@host` carries the token in the user field, so a rule that
	// spared colon-less userinfo would spare the most common leak. The cost is
	// that `ssh://git@host` also prints as `ssh://****@host`, which loses only a
	// constant.
	return raw[:i+3] + "****" + authority[at:]
}

func newSkillSourceListCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List SkillSources (or ClusterSkillSources with --cluster)",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := newSourceTable(g.Theme(out))
			n, anyProblem := 0, false

			if cluster {
				var list spiceboxv1alpha1.ClusterSkillSourceList
				if err := b.Controller.List(cmd.Context(), &list); err != nil {
					return err
				}
				n = len(list.Items)
				for i := range list.Items {
					s := &list.Items[i]
					anyProblem = anyProblem || len(s.Status.DiscoveryProblems) > 0
					sourceRow(t, s.Name, s.Spec.RepoURL, s.Spec.Ref, &s.Status, s.CreationTimestamp)
				}
			} else {
				var list spiceboxv1alpha1.SkillSourceList
				if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
					return err
				}
				n = len(list.Items)
				for i := range list.Items {
					s := &list.Items[i]
					anyProblem = anyProblem || len(s.Status.DiscoveryProblems) > 0
					sourceRow(t, s.Name, s.Spec.RepoURL, s.Spec.Ref, &s.Status, s.CreationTimestamp)
				}
			}

			kind := "SkillSources"
			if cluster {
				kind = "ClusterSkillSources"
			}
			if n == 0 {
				fmt.Fprintf(out, "no %s found\n", kind)
				return nil
			}
			fmt.Fprint(out, t.Render())
			if anyProblem {
				fmt.Fprintln(out, "\nSome sources skipped invalid SKILL.md files. Run `oap skill source show <name>"+clusterFlagHint(cluster)+"` for the reasons.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "List cluster-scoped ClusterSkillSources")
	return cmd
}

func newSkillSourceShowCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a SkillSource's spec, resolved SHA, discovery problems, and status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if cluster {
				var s spiceboxv1alpha1.ClusterSkillSource
				if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Name: args[0]}, &s); err != nil {
					return err
				}
				fmt.Fprintf(out, "Name:    %s\n", s.Name)
				fmt.Fprintf(out, "Repo:    %s\n", redactRepoURL(s.Spec.RepoURL))
				renderSourceCommon(out, s.Spec.Ref, s.Spec.Subpath, &s.Status)
				if s.Spec.Auth != nil {
					fmt.Fprintf(out, "Auth:    secret %s/%s key=%s\n", s.Spec.Auth.Namespace, s.Spec.Auth.SecretRef.Name, s.Spec.Auth.SecretRef.Key)
				}
				renderSourceStatus(out, &s.Status)
			} else {
				var s spiceboxv1alpha1.SkillSource
				if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &s); err != nil {
					return err
				}
				fmt.Fprintf(out, "Name:      %s\n", s.Name)
				fmt.Fprintf(out, "Namespace: %s\n", b.Namespace)
				fmt.Fprintf(out, "Repo:      %s\n", redactRepoURL(s.Spec.RepoURL))
				renderSourceCommon(out, s.Spec.Ref, s.Spec.Subpath, &s.Status)
				if s.Spec.Auth != nil {
					fmt.Fprintf(out, "Auth:      AgentIdentity %s credential %s\n", s.Spec.Auth.AgentIdentity, s.Spec.Auth.Credential)
				}
				renderSourceStatus(out, &s.Status)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "Target a cluster-scoped ClusterSkillSource")
	return cmd
}

func renderSourceCommon(out io.Writer, ref, subpath string, st *spiceboxv1alpha1.SkillSourceStatus) {
	if ref != "" {
		fmt.Fprintf(out, "Ref:       %s\n", ref)
	}
	if subpath != "" {
		fmt.Fprintf(out, "Subpath:   %s\n", subpath)
	}
	if st.ResolvedSHA != "" {
		fmt.Fprintf(out, "Resolved:  %s\n", st.ResolvedSHA)
	}
	fmt.Fprintf(out, "Skills:    %d discovered\n", st.DiscoveredSkills)
}

func renderSourceStatus(out io.Writer, st *spiceboxv1alpha1.SkillSourceStatus) {
	if len(st.DiscoveryProblems) > 0 {
		fmt.Fprintln(out, "Discovery problems (skills skipped, not materialized):")
		for _, p := range st.DiscoveryProblems {
			fmt.Fprintf(out, "  - %s\n", p)
		}
	}
	printConditionsBlock(out, st.Conditions)
}

func newSkillSourceCreateCmd(g *apcmd.Globals) *cobra.Command {
	var (
		repo, ref, subpath           string
		identity, credential         string
		secretNS, secretName, secKey string
		cluster                      bool
	)
	cmd := &cobra.Command{
		Use:   "create <name> --repo <url>",
		Short: "Create a SkillSource (or ClusterSkillSource with --cluster) from a git repo",
		Long: "Creates a SkillSource that clones <url> and materializes Skills from SKILL.md\n" +
			"files under --subpath. For a private repo, supply auth:\n" +
			"  namespaced: --identity <AgentIdentity> --credential <name>\n" +
			"  cluster:    --secret-namespace <ns> --secret-name <name> --secret-key <key>",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if repo == "" {
				return errors.New("--repo is required")
			}
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()

			if cluster {
				spec := spiceboxv1alpha1.ClusterSkillSourceSpec{RepoURL: repo, Ref: ref, Subpath: subpath}
				if secretName != "" {
					if secretNS == "" {
						return errors.New("--secret-namespace is required with --secret-name")
					}
					spec.Auth = &spiceboxv1alpha1.ClusterSkillSourceAuth{
						Namespace: secretNS,
						SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: secretName, Key: secKey},
					}
				}
				obj := &spiceboxv1alpha1.ClusterSkillSource{Spec: spec}
				obj.Name = args[0]
				if err := b.Controller.Create(cmd.Context(), obj); err != nil {
					return err
				}
				fmt.Fprintf(out, "created ClusterSkillSource %s (repo %s)\n", obj.Name, redactRepoURL(repo))
				return nil
			}

			spec := spiceboxv1alpha1.SkillSourceSpec{RepoURL: repo, Ref: ref, Subpath: subpath}
			if identity != "" || credential != "" {
				if identity == "" || credential == "" {
					return errors.New("--identity and --credential must be set together")
				}
				spec.Auth = &spiceboxv1alpha1.SkillSourceAuth{AgentIdentity: identity, Credential: credential}
			}
			obj := &spiceboxv1alpha1.SkillSource{Spec: spec}
			obj.Namespace = b.Namespace
			obj.Name = args[0]
			if err := b.Controller.Create(cmd.Context(), obj); err != nil {
				return err
			}
			fmt.Fprintf(out, "created SkillSource %s/%s (repo %s)\n", b.Namespace, obj.Name, redactRepoURL(repo))
			fmt.Fprintln(out, "Run `oap skill source show "+args[0]+"` to watch it sync, then `oap skill list` for the materialized Skills.")
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "Git repo URL to clone (required)")
	f.StringVar(&ref, "ref", "", "Branch, tag, or commit SHA to fetch (default: the repo's default branch)")
	f.StringVar(&subpath, "subpath", "", "Restrict SKILL.md discovery to this subtree (e.g. .claude/skills)")
	f.StringVar(&identity, "identity", "", "AgentIdentity holding the clone credential (namespaced)")
	f.StringVar(&credential, "credential", "", "AgentCredential name on the AgentIdentity (namespaced)")
	f.StringVar(&secretNS, "secret-namespace", "", "Namespace of the clone-token Secret (--cluster)")
	f.StringVar(&secretName, "secret-name", "", "Name of the clone-token Secret (--cluster)")
	f.StringVar(&secKey, "secret-key", "token", "Key in the clone-token Secret (--cluster)")
	f.BoolVar(&cluster, "cluster", false, "Create a cluster-scoped ClusterSkillSource")
	return cmd
}

func newSkillSourceDeleteCmd(g *apcmd.Globals) *cobra.Command {
	var cluster bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a SkillSource (or ClusterSkillSource with --cluster)",
		Long:  "Deleting a source garbage-collects the Skills it materialized (owner references).",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			if cluster {
				obj := &spiceboxv1alpha1.ClusterSkillSource{}
				obj.Name = args[0]
				if err := b.Controller.Delete(cmd.Context(), obj); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "deleted ClusterSkillSource %s\n", args[0])
				return nil
			}
			obj := &spiceboxv1alpha1.SkillSource{}
			obj.Namespace = b.Namespace
			obj.Name = args[0]
			if err := b.Controller.Delete(cmd.Context(), obj); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted SkillSource %s/%s\n", b.Namespace, args[0])
			return nil
		},
	}
	cmd.Flags().BoolVar(&cluster, "cluster", false, "Delete a cluster-scoped ClusterSkillSource")
	return cmd
}

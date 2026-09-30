//go:build mage
// +build mage

package main

import (
	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
)

// Fmt groups the source formatters.
type Fmt mg.Namespace

// oxfmtVersion pins the formatter. It is pinned exactly, with no caret, because
// a formatter's output is a contract: a floating version turns an upstream
// release into a failing check on an unrelated pull request, which is how a
// formatting gate gets switched off. The CI workflow pins the same version, so
// bump both together and land the reformat it produces in its own commit.
const oxfmtVersion = "0.70.0"

// fmtTargets are the paths oxfmt is pointed at. The globs are explicit rather
// than letting the formatter walk the tree, so a new file type cannot start
// being rewritten without someone adding it here. What each glob deliberately
// leaves out is in .oxfmtrc.json's ignorePatterns, including the generated MDX
// reference pages, the skill definitions that skillmd parses, and the built
// frontend bundles under webassets/dist.
var fmtTargets = []string{
	"**/*.md",
	"web/**/*.ts",
	"web/**/*.tsx",
	"showcase/**/*.ts",
	"showcase/**/*.tsx",
	"site/**/*.ts",
	"site/**/*.tsx",
}

// All rewrites Markdown and TypeScript in place.
func (Fmt) All() error {
	return sh.RunV("pnpm", oxfmtArgs("--write")...)
}

// Check reports files that are not formatted and exits non-zero if any are. It
// is what CI runs; Fmt.All is what fixes them.
func (Fmt) Check() error {
	return sh.RunV("pnpm", oxfmtArgs("--check")...)
}

// oxfmtArgs builds the `pnpm dlx` invocation. oxfmt ships as an npm package, and
// the root package.json exists only to pin pnpm for Vercel, with no
// dependencies — `pnpm dlx` pins the formatter's version without adding it
// there, and without pulling its dependency tree into go.mod.
func oxfmtArgs(mode string) []string {
	args := []string{"dlx", "oxfmt@" + oxfmtVersion, mode}
	return append(args, fmtTargets...)
}

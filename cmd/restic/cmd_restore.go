package main

import (
	"context"
	"time"

	"github.com/restic/restic/internal/debug"
	"github.com/restic/restic/internal/errors"
	"github.com/restic/restic/internal/restic"
	"github.com/restic/restic/internal/restorer"
	"github.com/restic/restic/internal/ui"
	restoreui "github.com/restic/restic/internal/ui/restore"
	"github.com/restic/restic/internal/ui/termstatus"

	"github.com/spf13/cobra"
)

var cmdRestore = &cobra.Command{
	Use:   "restore [flags] snapshotID",
	Short: "Extract the data from a snapshot",
	Long: `
The "restore" command extracts the data from a snapshot from the repository to
a directory.

The special snapshotID "latest" can be used to restore the latest snapshot in the
repository.

To only restore a specific subfolder, you can use the "<snapshotID>:<subfolder>"
syntax, where "subfolder" is a path within the snapshot.

EXIT STATUS
===========

Exit status is 0 if the command was successful, and non-zero if there was any error.
`,
	DisableAutoGenTag: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		term, cancel := setupTermstatus()
		defer cancel()
		return runRestore(cmd.Context(), restoreOptions, globalOptions, term, args)
	},
}

// RestoreOptions collects all options for the restore command.
type RestoreOptions struct {
	excludePatternOptions
	includePatternOptions
	Target string
	restic.SnapshotFilter
	Sparse bool
	Verify bool
}

var restoreOptions RestoreOptions

func init() {
	cmdRoot.AddCommand(cmdRestore)

	flags := cmdRestore.Flags()
	flags.StringVarP(&restoreOptions.Target, "target", "t", "", "directory to extract data to")

	initExcludePatternOptions(flags, &restoreOptions.excludePatternOptions)
	initIncludePatternOptions(flags, &restoreOptions.includePatternOptions)

	initSingleSnapshotFilter(flags, &restoreOptions.SnapshotFilter)
	flags.BoolVar(&restoreOptions.Sparse, "sparse", false, "restore files as sparse")
	flags.BoolVar(&restoreOptions.Verify, "verify", false, "verify restored files content")
}

func runRestore(ctx context.Context, opts RestoreOptions, gopts GlobalOptions,
	term *termstatus.Terminal, args []string) error {

	hasExcludes := len(opts.Excludes) > 0 || len(opts.InsensitiveExcludes) > 0
	hasIncludes := len(opts.Includes) > 0 || len(opts.InsensitiveIncludes) > 0

	excludePatternFns, err := opts.excludePatternOptions.CollectPatterns()
	if err != nil {
		return err
	}

	includePatternFns, err := opts.includePatternOptions.CollectPatterns()
	if err != nil {
		return err
	}

	switch {
	case len(args) == 0:
		return errors.Fatal("no snapshot ID specified")
	case len(args) > 1:
		return errors.Fatalf("more than one snapshot ID specified: %v", args)
	}

	if opts.Target == "" {
		return errors.Fatal("please specify a directory to restore to (--target)")
	}

	if hasExcludes && hasIncludes {
		return errors.Fatal("exclude and include patterns are mutually exclusive")
	}

	snapshotIDString := args[0]

	debug.Log("restore %v to %v", snapshotIDString, opts.Target)

	ctx, repo, unlock, err := openWithReadLock(ctx, gopts, gopts.NoLock)
	if err != nil {
		return err
	}
	defer unlock()

	sn, subfolder, err := (&restic.SnapshotFilter{
		Hosts: opts.Hosts,
		Paths: opts.Paths,
		Tags:  opts.Tags,
	}).FindLatest(ctx, repo, repo, snapshotIDString)
	if err != nil {
		return errors.Fatalf("failed to find snapshot: %v", err)
	}

	bar := newIndexTerminalProgress(gopts.Quiet, gopts.JSON, term)
	err = repo.LoadIndex(ctx, bar)
	if err != nil {
		return err
	}

	sn.Tree, err = restic.FindTreeDirectory(ctx, repo, sn.Tree, subfolder)
	if err != nil {
		return err
	}

	msg := ui.NewMessage(term, gopts.verbosity)
	var printer restoreui.ProgressPrinter
	if gopts.JSON {
		printer = restoreui.NewJSONProgress(term)
	} else {
		printer = restoreui.NewTextProgress(term)
	}

	progress := restoreui.NewProgress(printer, calculateProgressInterval(!gopts.Quiet, gopts.JSON))
	res := restorer.NewRestorer(repo, sn, opts.Sparse, progress)

	totalErrors := 0
	res.Error = func(location string, err error) error {
		msg.E("ignoring error for %s: %s\n", location, err)
		totalErrors++
		return nil
	}
	res.Warn = func(message string) {
		msg.E("Warning: %s\n", message)
	}

	selectExcludeFilter := func(item string, _ string, node *restic.Node) (selectedForRestore bool, childMayBeSelected bool) {
		for _, rejectFn := range excludePatternFns {
			matched := rejectFn(item)

			// An exclude filter is basically a 'wildcard but foo',
			// so even if a childMayMatch, other children of a dir may not,
			// therefore childMayMatch does not matter, but we should not go down
			// unless the dir is selected for restore
			selectedForRestore = !matched
			childMayBeSelected = selectedForRestore && node.Type == "dir"

			return selectedForRestore, childMayBeSelected
		}
		return selectedForRestore, childMayBeSelected
	}

	selectIncludeFilter := func(item string, _ string, node *restic.Node) (selectedForRestore bool, childMayBeSelected bool) {
		for _, includeFn := range includePatternFns {
			selectedForRestore, childMayBeSelected = includeFn(item)
		}

		childMayBeSelected = childMayBeSelected && node.Type == "dir"

		return selectedForRestore, childMayBeSelected
	}

	if hasExcludes {
		res.SelectFilter = selectExcludeFilter
	} else if hasIncludes {
		res.SelectFilter = selectIncludeFilter
	}

	if !gopts.JSON {
		msg.P("restoring %s to %s\n", res.Snapshot(), opts.Target)
	}

	err = res.RestoreTo(ctx, opts.Target)
	if err != nil {
		return err
	}

	progress.Finish()

	if totalErrors > 0 {
		return errors.Fatalf("There were %d errors\n", totalErrors)
	}

	if opts.Verify {
		if !gopts.JSON {
			msg.P("verifying files in %s\n", opts.Target)
		}
		var count int
		t0 := time.Now()
		count, err = res.VerifyFiles(ctx, opts.Target)
		if err != nil {
			return err
		}
		if totalErrors > 0 {
			return errors.Fatalf("There were %d errors\n", totalErrors)
		}

		if !gopts.JSON {
			msg.P("finished verifying %d files in %s (took %s)\n", count, opts.Target,
				time.Since(t0).Round(time.Millisecond))
		}
	}

	return nil
}

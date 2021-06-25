package new

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	errorutil "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/klog"

	"github.com/mfojtik/bugzilla-operator/pkg/cache"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/bugutil"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
)

type NewBugReporter struct {
	controller.ControllerContext
	config     config.OperatorConfig
	components []string
}

func NewNewBugReporter(ctx controller.ControllerContext, components, schedule []string, operatorConfig config.OperatorConfig, recorder events.Recorder) factory.Controller {
	c := &NewBugReporter{
		ctx,
		operatorConfig,
		components,
	}
	return factory.New().WithSync(c.sync).ResyncSchedule(schedule...).ToController("NewBugReporter", recorder)
}

func (c *NewBugReporter) sync(ctx context.Context, syncCtx factory.SyncContext) (err error) {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	stateKey := "new-bug-reporter.state-" + strings.Join(c.components, "-")
	lastID := 0
	if s, err := c.GetPersistentValue(ctx, stateKey); err != nil {
		return err
	} else if s != "" {
		lastID, err = strconv.Atoi(s)
		if err != nil {
			klog.Warningf("Cannot parse state value for %s: %v", stateKey, err)
			lastID = 0 // keep going
		}
	}
	defer func() {
		if persistErr := c.SetPersistentValue(ctx, stateKey, strconv.Itoa(lastID)); persistErr != nil {
			if err == nil {
				err = persistErr
			}
		}
	}()

	newBugs, err := getNewBugs(client, c.components, lastID)
	if err != nil {
		syncCtx.Recorder().Warningf("BuglistFailed", err.Error())
		return err
	}

	var errs []error
	ids := []string{}
	for i, b := range newBugs {
		if b.ID > lastID {
			lastID = b.ID
		}
		ids = append(ids, fmt.Sprintf("<https://bugzilla.redhat.com/show_bug.cgi?id=%d|#%d>", b.ID, b.ID))
		if i > 50 {
			ids = append(ids, fmt.Sprintf(" ... and %d more", len(newBugs)-50))
			break
		}
	}
	slackClient.MessageAdminChannel(fmt.Sprintf("Found new bugs: %s", strings.Join(ids, ", ")))

	// TODO: add interactivity and send to assignee

	return errorutil.NewAggregate(errs)
}

func Report(ctx context.Context, client cache.BugzillaClient, components []string) (string, error) {
	newBugs, err := getNewBugs(client, components, 0)
	if err != nil {
		return "", err
	}

	lines := []string{"New bugs of the last week (excluding those already in a different state):", ""}
	for i, b := range newBugs {
		lines = append(lines, fmt.Sprintf("> %s", bugutil.FormatBugMessage(*b)))
		if i > 20 {
			lines = append(lines, fmt.Sprintf(" ... and %d more", len(newBugs)-20))
			break
		}
	}

	return strings.Join(lines, "\n"), nil
}

func getNewBugs(client cache.BugzillaClient, components []string, lastID int) ([]*bugzilla.Bug, error) {
	aq := bugzilla.AdvancedQuery{
		Field: "bug_id",
		Op:    "greaterthan",
		Value: strconv.Itoa(lastID),
	}
	if lastID == 0 {
		aq = bugzilla.AdvancedQuery{
			Field: "creation_ts",
			Op:    "greaterthaneq",
			Value: "-24h", // last day
		}
	}

	return client.Search(bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW"},
		Component:      components,
		Advanced:       []bugzilla.AdvancedQuery{aq},
		IncludeFields: []string{
			"id",
			"assigned_to",
			"component",
			"summary",
		},
	})
}

package blockers

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/eparis/bugzilla"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/mfojtik/bugzilla-operator/pkg/cache"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/bugutil"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/config"
	"github.com/mfojtik/bugzilla-operator/pkg/operator/controller"
	"github.com/mfojtik/bugzilla-operator/pkg/slack"
)

type BlockersReporter struct {
	controller.ControllerContext
	config     config.OperatorConfig
	components []string
}

const (
	urgentIntro = "You have *%d urgent bugs*%s:\n\n"
	urgentOutro = "\n\nWe are expected to actively work on these before anything else!"

	blockerIntro = "You have *%d blocker+ bugs*%s:\n\n"
	blockerOutro = "\n\nPlease keep eyes on these, they will risk the upcoming release if not finished in time!"

	triageIntro = "You have *%d untriaged bugs*%s:\n\n"
	triageOutro = "\n\nPlease make sure all these have the _Severity_, _Priority_ and _Target Release_ set, and move to ASSIGNED, so I can stop bothering you :-)\n\n"
)

var (
	seriousKeywords = []string{
		"ServiceDeliveryBlocker",
		"TestBlocker",
		"UpgradeBlocker",
	}
)

func NewBlockersReporter(ctx controller.ControllerContext, components []string, schedule []string, operatorConfig config.OperatorConfig,
	recorder events.Recorder) factory.Controller {
	c := &BlockersReporter{
		ControllerContext: ctx,
		config:            operatorConfig,
		components:        components,
	}
	return factory.New().WithSync(c.sync).ResyncSchedule(schedule...).ToController("BlockersReporter", recorder)
}

type bugSummary struct {
	seriousIDs             map[string][]int
	blockerPlus            []string
	blockerPlusIDs         []int
	blockerQuestionmark    []string
	blockerQuestionmarkIDs []int
	toTriage               []string
	toTriageIDs            []int
	needUpcomingSprint     []string
	needUpcomingSprintIDs  []int
	urgentIDs              []int
	urgent                 []string
	staleCount             int
	priorityCount          map[string]int
	severityCount          map[string]int
	currentReleaseCount    int
}

func summarizeBugs(currentTargetRelease string, bugs ...*bugzilla.Bug) bugSummary {
	r := bugSummary{
		priorityCount: map[string]int{},
		severityCount: map[string]int{},
		seriousIDs:    map[string][]int{},
	}
	for _, bug := range bugs {
		keywords := sets.NewString(bug.Keywords...)
		for _, keyword := range seriousKeywords {
			if keywords.Has(keyword) {
				r.seriousIDs[keyword] = append(r.seriousIDs[keyword], bug.ID)
			}
		}

		if strings.Contains(bug.Whiteboard, "LifecycleStale") {
			r.staleCount++
		}

		r.severityCount[bug.Severity]++
		r.priorityCount[bug.Priority]++

		if bug.Priority == "urgent" || bug.Severity == "urgent" {
			r.urgent = append(r.urgent, bugutil.FormatBugMessage(*bug))
			r.urgentIDs = append(r.urgentIDs, bug.ID)
		}

		if !keywords.Has("UpcomingSprint") {
			r.needUpcomingSprint = append(r.needUpcomingSprint, bugutil.FormatBugMessage(*bug))
			r.needUpcomingSprintIDs = append(r.needUpcomingSprintIDs, bug.ID)
		}

		targetRelease := "---"
		if len(bug.TargetRelease) > 0 {
			targetRelease = bug.TargetRelease[0]
		}

		if hasFlag(bug, "blocker", "+") && (targetRelease == currentTargetRelease || targetRelease == "---") {
			r.blockerPlus = append(r.blockerPlus, bugutil.FormatBugMessage(*bug))
			r.blockerPlusIDs = append(r.blockerPlusIDs, bug.ID)
			r.seriousIDs["blocker+"] = append(r.seriousIDs["blocker+"], bug.ID)
		}

		if hasFlag(bug, "blocker", "?") && (targetRelease == currentTargetRelease || targetRelease == "---") {
			r.blockerQuestionmark = append(r.blockerQuestionmark, bugutil.FormatBugMessage(*bug))
			r.blockerQuestionmarkIDs = append(r.blockerQuestionmarkIDs, bug.ID)
			r.seriousIDs["blocker?"] = append(r.seriousIDs["blocker?"], bug.ID)
		}

		triageState := sets.NewString("NEW", "")
		if (targetRelease == currentTargetRelease && triageState.Has(bug.Status)) || targetRelease == "---" || bug.Priority == "unspecified" || bug.Priority == "" || bug.Severity == "unspecified" || bug.Severity == "" {
			r.toTriage = append(r.toTriage, bugutil.FormatBugMessage(*bug))
			r.toTriageIDs = append(r.toTriageIDs, bug.ID)
		}

		if targetRelease == currentTargetRelease || targetRelease == "---" {
			r.currentReleaseCount++
		}
	}

	return r
}

func hasFlag(bug *bugzilla.Bug, name, value string) bool {
	for _, f := range bug.Flags {
		if f.Name == name && f.Status == value {
			return true
		}
	}
	return false
}

func (c *BlockersReporter) sync(ctx context.Context, syncCtx factory.SyncContext) error {
	client := c.NewBugzillaClient(ctx)
	slackClient := c.SlackClient(ctx)

	channelReport, summary, bugs, err := Report(ctx, client, syncCtx.Recorder(), &c.config, c.components)
	if err != nil {
		return err
	}

	byID := map[int]*bugzilla.Bug{}
	for _, b := range bugs {
		byID[b.ID] = b
	}

	perPerson := func(ids []int, lines []string) (map[string][]int, map[string][]string) {
		perPersonLines := map[string][]string{}
		perPersonIDs := map[string][]int{}
		for i, id := range ids {
			b, ok := byID[id]
			if !ok {
				continue
			}
			perPersonLines[b.AssignedTo] = append(perPersonLines[b.AssignedTo], lines[i])
			perPersonIDs[b.AssignedTo] = append(perPersonIDs[b.AssignedTo], id)
		}
		return perPersonIDs, perPersonLines
	}

	perPersonToTriageIDs, perPersonToTriage := perPerson(summary.toTriageIDs, summary.toTriage)
	perPersonBlockerPlusIDs, perPersonBlockerPlus := perPerson(summary.toTriageIDs, summary.toTriage)
	perPersonUrgentIDs, perPersonUrgent := perPerson(summary.urgentIDs, summary.urgent)

	notifyPersons := func(intro, suffix string, perPersonBugs map[string][]string, outro string) {
		for person, lines := range perPersonBugs {
			if len(lines) == 0 {
				continue
			}
			message := fmt.Sprintf("%s%s%s", fmt.Sprintf(intro, len(lines), suffix), strings.Join(lines, "\n"), outro)
			if err := slackClient.MessageEmail(person, message); err != nil {
				syncCtx.Recorder().Warningf("DeliveryFailed", "Failed to deliver:\n\n%s\n\n to %q: %v", message, person, err)
			}
		}
	}

	notifyPersons(triageIntro, "", perPersonToTriage, triageOutro)
	notifyPersons(blockerIntro, fmt.Sprintf("for the %s release", c.config.Release.CurrentTargetRelease), perPersonBlockerPlus, blockerOutro)
	notifyPersons(urgentIntro, "", perPersonUrgent, urgentOutro)

	if err := slackClient.MessageChannel(channelReport); err != nil {
		syncCtx.Recorder().Warningf("DeliveryFailed", "Failed to deliver stats to channel: %v", err)
	}

	// send debug stats
	c.sendAdminDebugStats(slackClient, perPersonBlockerPlusIDs, perPersonToTriageIDs, perPersonUrgentIDs)
	return nil
}

func getBugsQuery(config *config.OperatorConfig, components []string, targetRelease []string) bugzilla.Query {
	return bugzilla.Query{
		Classification: []string{"Red Hat"},
		Product:        []string{"OpenShift Container Platform"},
		Status:         []string{"NEW", "ASSIGNED", "POST", "ON_DEV"},
		Component:      components,
		TargetRelease:  targetRelease,
		Advanced: []bugzilla.AdvancedQuery{
			{
				Field: "bug_severity",
				Op:    "notequals",
				Value: "low",
			},
			{
				Field: "priority",
				Op:    "notequals",
				Value: "low",
			},
		},
		IncludeFields: []string{
			"id",
			"assigned_to",
			"keywords",
			"status",
			"resolution",
			"summary",
			"changeddate",
			"severity",
			"priority",
			"target_release",
			"whiteboard",
			"flags",
		},
	}
}

func Report(ctx context.Context, client cache.BugzillaClient, recorder events.Recorder, config *config.OperatorConfig, components []string) (string, *bugSummary, []*bugzilla.Bug, error) {
	allReleasesQuery := getBugsQuery(config, components, append([]string{"---"}, config.Release.TargetReleases...))
	currentReleaseQeury := getBugsQuery(config, components, append([]string{"---"}, config.Release.CurrentTargetRelease))

	bugs, err := client.Search(allReleasesQuery)
	if err != nil {
		recorder.Warningf("BugSearchFailed", err.Error())
		return "", nil, nil, err
	}

	summary := summarizeBugs(config.Release.CurrentTargetRelease, bugs...)
	channelStats := getStatsForChannel(
		config.Release.CurrentTargetRelease,
		len(bugs),
		summary,
		allReleasesQuery,
		currentReleaseQeury,
	)

	report := fmt.Sprintf("\n:bug: *Today 4.x Bug Report:* :bug:\n%s\n", strings.Join(channelStats, "\n"))
	return report, &summary, nil, nil
}

func makeBugzillaLink(hrefText string, ids ...int) string {
	u, _ := url.Parse("https://bugzilla.redhat.com/buglist.cgi?f1=bug_id&list_id=11100046&o1=anyexact&query_format=advanced")
	e := u.Query()
	stringIds := make([]string, len(ids))
	for i := range stringIds {
		stringIds[i] = fmt.Sprintf("%d", ids[i])
	}
	e.Add("v1", strings.Join(stringIds, ","))
	u.RawQuery = e.Encode()
	return fmt.Sprintf("<%s|%s>", u.String(), hrefText)
}

func (c *BlockersReporter) sendAdminDebugStats(slackClient slack.ChannelClient, perPersonBlockersIDs, perPersonToTriageIDs, perPersonUrgentIDs map[string][]int) {
	var messages []string
	for person, b := range perPersonBlockersIDs {
		if len(b) > 0 {
			messages = append(messages, fmt.Sprintf("> %s: %d blocker+ bugs", makeBugzillaLink(person, perPersonBlockersIDs[person]...), len(b)))
		}
	}
	for person, b := range perPersonToTriageIDs {
		if len(b) > 0 {
			messages = append(messages, fmt.Sprintf("> %s: %d bugs that need triage", makeBugzillaLink(person, perPersonToTriageIDs[person]...), len(b)))
		}
	}
	for person, b := range perPersonUrgentIDs {
		if len(b) > 0 {
			messages = append(messages, fmt.Sprintf("> %s: %d urgent bugs", makeBugzillaLink(person, perPersonUrgentIDs[person]...), len(b)))
		}
	}
	slackClient.MessageAdminChannel(strings.Join(messages, "\n"))
}

func getStatsForChannel(targetRelease string, activeBugsCount int, summary bugSummary, allReleasesQuery, currentReleaseQuery bugzilla.Query) []string {
	sortedPrioNames := []string{
		"urgent",
		"high",
		"medium",
		"low",
		"unspecified",
	}
	severityMessages := []string{}
	for _, p := range sortedPrioNames {
		if summary.severityCount[p] > 0 {
			severityMessages = append(severityMessages, fmt.Sprintf("%d _%s_", summary.severityCount[p], p))
		}
	}
	priorityMessages := []string{}
	for _, p := range sortedPrioNames {
		if summary.priorityCount[p] > 0 {
			priorityMessages = append(priorityMessages, fmt.Sprintf("%d _%s_", summary.priorityCount[p], p))
		}
	}

	allReleasesQueryURL, _ := url.Parse("https://bugzilla.redhat.com/buglist.cgi?" + allReleasesQuery.Values().Encode())
	currentReleaseQueryURL, _ := url.Parse("https://bugzilla.redhat.com/buglist.cgi?" + currentReleaseQuery.Values().Encode())

	lines := []string{
		fmt.Sprintf("> All active 4.x and 3.11 Bugs: <%s|%d>", allReleasesQueryURL.String(), activeBugsCount),
		fmt.Sprintf("> All active %s Bugs: <%s|%d>", targetRelease, currentReleaseQueryURL.String(), summary.currentReleaseCount),
		fmt.Sprintf("> Bugs Severity Breakdown: %s", strings.Join(severityMessages, ", ")),
		fmt.Sprintf("> Bugs Priority Breakdown: %s", strings.Join(priorityMessages, ", ")),
		fmt.Sprintf("> Bugs Marked as _LifecycleStale_: <https://bugzilla.redhat.com/buglist.cgi?cmdtype=dorem&remaction=run&namedcmd=openshift-group-b-lifecycle-stale&sharer_id=290313|%d>", summary.staleCount),
	}

	for keyword, ids := range summary.seriousIDs {
		if len(ids) > 0 {
			keywordURL := makeBugzillaLink(fmt.Sprintf("%d", len(ids)), ids...)
			lines = append(lines, fmt.Sprintf("> Bugs with _%s_: %s", keyword, keywordURL))
		}
	}

	return lines
}

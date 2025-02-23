package operator

import (
	"fmt"

	"k8s.io/klog"

	"github.com/openshift-eng/shodan/pkg/operator/config"
	"github.com/openshift-eng/shodan/pkg/slack"
	"github.com/openshift-eng/shodan/pkg/slacker"
)

func auth(cfg config.OperatorConfig, handler func(req slacker.Request, w slacker.ResponseWriter), restrictedTo ...string) func(req slacker.Request, w slacker.ResponseWriter) {
	users := config.ExpandGroups(cfg.Groups, restrictedTo...)

	return func(req slacker.Request, w slacker.ResponseWriter) {
		denied := func() {
			w.Reply(fmt.Sprintf("Permission denied: User %q (%q) does not have permission to run this command", req.Event().Username, req.Event().User))
		}
		if len(req.Event().Username) == 0 || !users.Has(req.Event().Username) {
			u, err := w.Client().GetUserInfo(req.Event().User)
			if err != nil {
				denied()
				klog.Error(err)
				return
			}

			if len(u.Profile.Email) == 0 || !users.Has(slack.SlackEmailToBugzilla(&cfg, u.Profile.Email)) {
				denied()
				klog.Errorf("Denied to: %#v", u)
				return
			}
		}

		handler(req, w)
	}
}

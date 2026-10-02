package vercel

import (
	"context"
	"testing"
	"time"

	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/vercel"
)

// The handler turns the wire's minutes and filters into one query.
func TestLogsPassesTheQuery(t *testing.T) {
	var got vercel.LogQuery
	d := Deps{ReadLogs: func(_ context.Context, q vercel.LogQuery) (vercel.LogPage, error) {
		got = q
		return vercel.LogPage{}, nil
	}}
	_, err := d.logs(t.Context(), rpc.Caller{}, LogsParams{
		TeamID: "team_a", ProjectID: "prj_1", Environment: "preview", Level: "error", StatusCode: "500",
		WindowMinutes: 15, Cursor: "1000:a", Limit: 50,
	})
	want := vercel.LogQuery{TeamID: "team_a", ProjectID: "prj_1", Environment: "preview", Level: "error", StatusCode: "500",
		Window: 15 * time.Minute, Cursor: "1000:a", Limit: 50}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestDeploymentsPassesTheQuery(t *testing.T) {
	var got vercel.DeploymentQuery
	d := Deps{ReadDeployments: func(_ context.Context, q vercel.DeploymentQuery) (vercel.DeploymentPage, error) {
		got = q
		return vercel.DeploymentPage{}, nil
	}}
	_, err := d.deployments(t.Context(), rpc.Caller{}, DeploymentsParams{
		TeamID: "team_a", ProjectID: "prj_1", Environment: "production", State: "error", Cursor: "690", Limit: 10,
	})
	want := vercel.DeploymentQuery{TeamID: "team_a", ProjectID: "prj_1", Environment: "production", State: "error", Cursor: "690", Limit: 10}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
}

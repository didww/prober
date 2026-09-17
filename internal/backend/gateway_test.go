package backend

import (
	"log/slog"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/didww/prober/api/gen/prober/v1"
)

func TestAuthSite(t *testing.T) {
	gw := NewGateway(Config{
		AgentToken:   "shared",
		AllowedSites: []string{"fra", "ams"},
		Agents:       []AgentAuth{{Site: "nyc", Token: "pinned-nyc"}},
	}, slog.New(slog.NewTextHandler(nil, nil)))

	hello := func(site string) *pb.Hello { return &pb.Hello{Site: site} }

	cases := []struct {
		name    string
		tok     string
		hello   *pb.Hello
		want    string
		wantErr codes.Code
	}{
		{"pinned token wins, hello ignored", "pinned-nyc", hello("elsewhere"), "nyc", codes.OK},
		{"shared token registers declared site", "shared", hello("fra"), "fra", codes.OK},
		{"shared token, other allowed site", "shared", hello("ams"), "ams", codes.OK},
		{"shared token, site not allowed", "shared", hello("hk"), "", codes.PermissionDenied},
		{"shared token, empty site", "shared", hello(""), "", codes.InvalidArgument},
		{"unknown token", "nope", hello("fra"), "", codes.Unauthenticated},
		{"empty token", "", hello("fra"), "", codes.Unauthenticated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site, err := gw.authSite(tc.tok, tc.hello)
			if tc.wantErr == codes.OK {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if site != tc.want {
					t.Fatalf("site = %q, want %q", site, tc.want)
				}
				return
			}
			if status.Code(err) != tc.wantErr {
				t.Fatalf("error code = %v, want %v (site %q)", status.Code(err), tc.wantErr, site)
			}
		})
	}
}

func TestAuthSiteSharedUnconstrained(t *testing.T) {
	// No allowed_sites: the shared token accepts any non-empty site.
	gw := NewGateway(Config{AgentToken: "s"}, slog.New(slog.NewTextHandler(nil, nil)))
	site, err := gw.authSite("s", &pb.Hello{Site: "anywhere"})
	if err != nil || site != "anywhere" {
		t.Fatalf("site=%q err=%v", site, err)
	}
}

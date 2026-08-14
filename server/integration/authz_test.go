package integration

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/rahul-sharma-cs/drive/server/internal/testutil"
)

// The authorization matrix (PLAN §Testing 2).
//
// Every endpoint class Phase 1 implements, crossed with every identity that can
// reach it, asserting the status AND -- on every rejection -- that the database
// did not move. A 404 with a row written behind it is not a rejection, and only
// the digest catches that.
//
// The destination rows are the point of the exercise. PLAN's IDOR rule runs in
// both directions: an endpoint that takes a parent in the body authorizes that
// parent independently of the resource in the path, so "move my node into your
// folder", "copy my file into your folder" and "create a folder inside your
// folder" are all 404 with nothing written, even though the caller does own the
// thing in the path.
//
// Extending it: `role` is the axis. Phase 3 adds guest (a share guest session,
// including the guest-of-share-A-against-share-B row) and Phase 6 adds bearer
// (PAT scopes); each is one more entry in the actors slice plus one `want`
// field per case. Nothing else in this file has to change.

// role is one identity in the matrix.
type role string

const (
	roleOwner role = "owner" // owns the node in the path
	roleOther role = "other" // a different signed-in user
	roleAnon  role = "anon"  // signed out
	// Phase 3: roleGuest (share guest session). Phase 6: roleBearer (PAT).
)

// want is one expected outcome: a status, and the error envelope's code when
// the status is a rejection.
type want struct {
	status int
	code   string
}

var (
	wantNotFound     = want{http.StatusNotFound, "not_found"}
	wantUnauthorized = want{http.StatusUnauthorized, "unauthorized"}
)

// scene is the fixture set one matrix case runs against. It is rebuilt per
// case, so a case whose owner row legitimately trashes or purges something
// cannot disturb the next one.
type scene struct {
	ownerFolder  uuid.UUID // owner's folder; holds ownerFile
	ownerSpare   uuid.UUID // a second folder of owner's: a legal destination
	ownerFile    uuid.UUID // a real file with real bytes in Garage
	ownerTrashed uuid.UUID // already in the trash, for restore and purge
	otherFolder  uuid.UUID // the forbidden destination
	marker       string    // a name unique to this scene, for the search row
}

type authzCase struct {
	name   string
	method string
	path   func(sc scene) string
	body   func(sc scene) any

	owner want
	other want // zero value means the standard 404 not_found
	anon  want // zero value means the standard 401 unauthorized

	// restoresSession marks the logout row, after which the two signed-in
	// clients have to sign back in.
	restoresSession bool
}

func TestAuthzMatrix(t *testing.T) {
	owner := H.NewUser(t)
	other := H.NewUser(t)
	anon := H.Anonymous(t)

	cases := []authzCase{
		{
			name:   "GET /nodes/{id}",
			method: http.MethodGet,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFolder.String() },
			owner:  want{status: http.StatusOK},
		},
		{
			name:   "GET /nodes/{id}/children",
			method: http.MethodGet,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFolder.String() + "/children" },
			owner:  want{status: http.StatusOK},
		},
		{
			name:   "POST /folders under own folder",
			method: http.MethodPost,
			path:   func(scene) string { return "/api/folders" },
			body: func(sc scene) any {
				return map[string]any{"parent_id": sc.ownerFolder, "name": "child"}
			},
			owner: want{status: http.StatusCreated},
		},
		{
			// Destination row: the caller owns nothing in the path -- the
			// parent in the body is the whole request, and it is not theirs.
			name:   "POST /folders under ANOTHER USER's folder",
			method: http.MethodPost,
			path:   func(scene) string { return "/api/folders" },
			body: func(sc scene) any {
				return map[string]any{"parent_id": sc.otherFolder, "name": "intruder"}
			},
			owner: wantNotFound,
			// For `other` this is their own folder, so it succeeds: the check
			// is ownership, not a blanket refusal of the id.
			other: want{status: http.StatusCreated},
		},
		{
			name:   "PATCH /nodes/{id} rename",
			method: http.MethodPatch,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFile.String() },
			body:   func(scene) any { return map[string]any{"name": "renamed.bin"} },
			owner:  want{status: http.StatusOK},
		},
		{
			// Destination row: move own node into another user's folder.
			name:   "PATCH /nodes/{id} move into ANOTHER USER's folder",
			method: http.MethodPatch,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFile.String() },
			body:   func(sc scene) any { return map[string]any{"parent_id": sc.otherFolder} },
			owner:  wantNotFound,
		},
		{
			name:   "POST /nodes/{id}/copy into own folder",
			method: http.MethodPost,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFile.String() + "/copy" },
			body:   func(sc scene) any { return map[string]any{"parent_id": sc.ownerSpare} },
			owner:  want{status: http.StatusCreated},
		},
		{
			// Destination row: copy own file into another user's folder. The
			// digest covers blobs.refcount, so a refcount bumped before the
			// destination check would fail here even though no node appeared.
			name:   "POST /nodes/{id}/copy into ANOTHER USER's folder",
			method: http.MethodPost,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFile.String() + "/copy" },
			body:   func(sc scene) any { return map[string]any{"parent_id": sc.otherFolder} },
			owner:  wantNotFound,
		},
		{
			name:   "DELETE /nodes/{id} (trash)",
			method: http.MethodDelete,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerFolder.String() },
			owner:  want{status: http.StatusNoContent},
		},
		{
			name:   "POST /nodes/{id}/restore",
			method: http.MethodPost,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerTrashed.String() + "/restore" },
			owner:  want{status: http.StatusOK},
		},
		{
			name:   "DELETE /nodes/{id}/purge",
			method: http.MethodDelete,
			path:   func(sc scene) string { return "/api/nodes/" + sc.ownerTrashed.String() + "/purge" },
			owner:  want{status: http.StatusNoContent},
		},
		{
			// No path resource: every signed-in caller sees their own trash,
			// and the isolation assertion is TestListEndpointsNeverLeakAnotherUsersNodes.
			name:   "GET /trash",
			method: http.MethodGet,
			path:   func(scene) string { return "/api/trash" },
			owner:  want{status: http.StatusOK},
			other:  want{status: http.StatusOK},
		},
		{
			name:   "GET /search",
			method: http.MethodGet,
			path:   func(sc scene) string { return "/api/search?q=" + url.QueryEscape(sc.marker) },
			owner:  want{status: http.StatusOK},
			other:  want{status: http.StatusOK},
		},
		{
			name:   "GET /auth/me",
			method: http.MethodGet,
			path:   func(scene) string { return "/api/auth/me" },
			owner:  want{status: http.StatusOK},
			other:  want{status: http.StatusOK},
		},
		{
			// Logout deliberately sits outside RequireAuth: a client that has
			// lost track of its session still ends up logged out, so anonymous
			// gets 204 rather than 401.
			name:            "POST /auth/logout",
			method:          http.MethodPost,
			path:            func(scene) string { return "/api/auth/logout" },
			owner:           want{status: http.StatusNoContent},
			other:           want{status: http.StatusNoContent},
			anon:            want{status: http.StatusNoContent},
			restoresSession: true,
		},
	}

	clients := map[role]*testutil.Client{roleOwner: owner, roleOther: other, roleAnon: anon}

	for i, c := range cases {
		// Rejections are asserted against the digest, so the cases cannot run
		// in parallel: they share the two identities the digest is scoped to.
		t.Run(c.name, func(t *testing.T) {
			sc := buildScene(t, i, owner, other)

			// Anonymous first, then the other user, then the owner -- whose
			// row is the only one that may legitimately destroy the scene.
			for _, role := range []role{roleAnon, roleOther, roleOwner} {
				w := c.expected(role)
				before := testutil.Digest(t, H.Pool, owner.ID, other.ID)

				resp := clients[role].Do(t, c.method, c.path(sc), bodyOf(c, sc))
				if resp.Status != w.status {
					t.Errorf("%s: status %d, want %d (body %s)", role, resp.Status, w.status, resp.Body)
				}
				if w.code != "" && resp.Code() != w.code {
					t.Errorf("%s: code %q, want %q (body %s)", role, resp.Code(), w.code, resp.Body)
				}
				if w.status >= 400 {
					if after := testutil.Digest(t, H.Pool, owner.ID, other.ID); after != before {
						t.Errorf("%s: the request was rejected with %d but the database changed", role, resp.Status)
					}
				}
			}

			if c.restoresSession {
				owner.Login(t)
				other.Login(t)
			}
		})
	}
}

// expected fills in the two defaults every path-resource endpoint shares: a
// different signed-in user gets 404 (never 403 -- the API must not confirm that
// someone else's node exists) and a signed-out caller gets 401.
func (c authzCase) expected(r role) want {
	switch r {
	case roleOwner:
		return c.owner
	case roleOther:
		if c.other == (want{}) {
			return wantNotFound
		}
		return c.other
	default:
		if c.anon == (want{}) {
			return wantUnauthorized
		}
		return c.anon
	}
}

func bodyOf(c authzCase, sc scene) any {
	if c.body == nil {
		return nil
	}
	return c.body(sc)
}

// buildScene creates one case's fixtures: two folders and a real file for the
// owner, one already-trashed folder, and a folder belonging to the other user
// that every destination row aims at. Names carry the case index so cases never
// collide in a shared parent.
func buildScene(t *testing.T, i int, owner, other *testutil.Client) scene {
	t.Helper()
	marker := fmt.Sprintf("authz-%d-%s", i, uuid.NewString()[:8])

	sc := scene{marker: marker}
	sc.ownerFolder = owner.CreateFolder(t, owner.RootID, marker+"-folder").ID
	sc.ownerSpare = owner.CreateFolder(t, owner.RootID, marker+"-spare").ID
	sc.ownerFile = H.CreateFile(t, owner.ID, sc.ownerFolder, marker+"-file.bin", []byte("authz fixture bytes"))
	sc.otherFolder = other.CreateFolder(t, other.RootID, marker+"-others-folder").ID

	trashed := owner.CreateFolder(t, owner.RootID, marker+"-trashed")
	owner.Delete(t, "/api/nodes/"+trashed.ID.String()).Expect(http.StatusNoContent)
	sc.ownerTrashed = trashed.ID

	return sc
}

// X-Drive-Client is required on every /api mutation. It is checked before
// authentication, so these cases are deliberately separate from the matrix
// above: a missing header is a 403 whether or not the caller is signed in, and
// mixing the two would let a routing mistake pass as an authz result.
//
// One case per route group with a mutation. Search has none -- it is GET only
// -- so it does not appear.
func TestMutationsRequireTheClientHeader(t *testing.T) {
	owner := H.NewUser(t)
	bare := owner.WithoutClientHeader()

	folder := owner.CreateFolder(t, owner.RootID, "csrf-"+uuid.NewString()[:8])
	trashed := owner.CreateFolder(t, owner.RootID, "csrf-trashed-"+uuid.NewString()[:8])
	owner.Delete(t, "/api/nodes/"+trashed.ID.String()).Expect(http.StatusNoContent)

	cases := []struct {
		group  string
		method string
		path   string
		body   any
	}{
		{"auth", http.MethodPost, "/api/auth/logout", nil},
		{"folders", http.MethodPost, "/api/folders", map[string]any{"parent_id": owner.RootID, "name": "nope"}},
		{"nodes", http.MethodPatch, "/api/nodes/" + folder.ID.String(), map[string]any{"name": "nope"}},
		{"copy", http.MethodPost, "/api/nodes/" + folder.ID.String() + "/copy", map[string]any{"parent_id": owner.RootID}},
		{"trash", http.MethodDelete, "/api/nodes/" + folder.ID.String(), nil},
		{"restore", http.MethodPost, "/api/nodes/" + trashed.ID.String() + "/restore", nil},
		{"purge", http.MethodDelete, "/api/nodes/" + trashed.ID.String() + "/purge", nil},
	}

	for _, c := range cases {
		t.Run(c.group, func(t *testing.T) {
			before := testutil.Digest(t, H.Pool, owner.ID)

			resp := bare.Do(t, c.method, c.path, c.body)
			if resp.Status != http.StatusForbidden {
				t.Fatalf("status %d, want 403 (body %s)", resp.Status, resp.Body)
			}
			if resp.Code() != "invalid" {
				t.Errorf("code %q, want invalid", resp.Code())
			}
			if after := testutil.Digest(t, H.Pool, owner.ID); after != before {
				t.Error("the CSRF gate rejected the request but the database changed")
			}
		})
	}

	// The same requests with the header are not 403 -- otherwise the cases
	// above would pass against a broken route just as happily.
	owner.Patch(t, "/api/nodes/"+folder.ID.String(), map[string]any{"name": "allowed"}).Expect(http.StatusOK)
}

// The list endpoints take no node id, so the matrix cannot express their
// isolation. They still must never show another user's rows.
func TestListEndpointsNeverLeakAnotherUsersNodes(t *testing.T) {
	owner := H.NewUser(t)
	other := H.NewUser(t)

	marker := "leak-" + uuid.NewString()[:8]
	live := owner.CreateFolder(t, owner.RootID, marker+"-live")
	trashed := owner.CreateFolder(t, owner.RootID, marker+"-trashed")
	owner.Delete(t, "/api/nodes/"+trashed.ID.String()).Expect(http.StatusNoContent)

	// The owner sees both, so the query itself works.
	if names := owner.Get(t, "/api/search?q="+url.QueryEscape(marker)).Expect(http.StatusOK).List().Names(); !contains(names, live.Name) {
		t.Fatalf("the owner's own search for %q did not find %q: %v", marker, live.Name, names)
	}
	if names := owner.Get(t, "/api/trash").Expect(http.StatusOK).List().Names(); !contains(names, trashed.Name) {
		t.Fatalf("the owner's trash does not list %q: %v", trashed.Name, names)
	}

	// The other user sees neither.
	if got := other.Get(t, "/api/search?q="+url.QueryEscape(marker)).Expect(http.StatusOK).List(); len(got.Items) != 0 {
		t.Errorf("another user's search returned %v", got.Names())
	}
	if names := other.Get(t, "/api/trash").Expect(http.StatusOK).List().Names(); contains(names, trashed.Name) {
		t.Errorf("another user's trash listed %q: %v", trashed.Name, names)
	}
	if got := other.Get(t, "/api/nodes/"+owner.RootID.String()+"/children"); got.Status != http.StatusNotFound {
		t.Errorf("listing another user's root children: status %d, want 404", got.Status)
	}
}

// A copy into another user's folder must not touch the source blob. The matrix
// digest already covers this; asserting the refcount directly says what the
// digest is protecting.
func TestRejectedCopyLeavesTheBlobRefcountAlone(t *testing.T) {
	owner := H.NewUser(t)
	other := H.NewUser(t)

	file := H.CreateFile(t, owner.ID, owner.RootID, "refcount-"+uuid.NewString()[:8]+".bin", []byte("bytes"))
	victim := other.CreateFolder(t, other.RootID, "victim-"+uuid.NewString()[:8])

	if got := H.Refcount(t, file); got != 1 {
		t.Fatalf("refcount before = %d, want 1", got)
	}

	resp := owner.Post(t, "/api/nodes/"+file.String()+"/copy", map[string]any{"parent_id": victim.ID})
	resp.Expect(http.StatusNotFound)

	if got := H.Refcount(t, file); got != 1 {
		t.Errorf("refcount after the rejected copy = %d, want 1", got)
	}
	if n := H.CountRows(t, "nodes", "owner_id = $1 AND parent_id = $2", other.ID, victim.ID); n != 0 {
		t.Errorf("%d node(s) appeared in the victim's folder", n)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

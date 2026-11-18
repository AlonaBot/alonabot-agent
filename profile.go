package agent

import (
	"encoding/json"
	"strings"

	pb "github.com/AlonaBot/AlonaBot/contracts/pb/alonabotv1"
	"github.com/AlonaBot/AlonaBot/sdk"
	"github.com/AlonaBot/AlonaBot/sdk/service"
)

var (
	profileList    = service.Proto[*pb.ProfileListRequest, *pb.ProfileListResponse]("core.profile.list")
	profileCurrent = service.Proto[*pb.ProfileCurrentRequest, *pb.ProfileCurrentResponse]("core.profile.current")
	profileAssign  = service.Proto[*pb.ProfileAssignRequest, *pb.ProfileAssignResponse]("core.profile.assign")
	profileSet     = service.Proto[*pb.ProfileSetRequest, *pb.ProfileSetResponse]("core.profile.set")
)

func setProfile(c *sdk.Context, path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	response, err := profileSet.Call(c.Context(), &pb.ProfileSetRequest{Path: path, ValueJson: string(raw)})
	if err != nil {
		return err
	}
	text := c.T("agent.profile_updated", "name", response.Profile.Name, "sessions", response.Profile.Sessions)
	if response.Reloading {
		text += "\n" + c.T("agent.profile_reloading")
	}
	// default 的重载等待整个根调用释放，回复必须在当前 Handler 内完成。
	return c.Reply(text)
}

func (a *component) selectProfile(c *sdk.Context, name string) error {
	c.StopPropagation()
	if name != "" {
		id := ""
		if name != "默认" && name != "default" {
			response, err := profileList.Call(c.Context(), &pb.ProfileListRequest{})
			if err != nil {
				return err
			}
			var names []string
			for _, profile := range response.Profiles {
				names = append(names, profile.Name)
				if profile.Name == name {
					id = profile.Id
				}
			}
			if id == "" {
				return c.Reply(c.T("agent.unknown_profile", "name", name, "names", strings.Join(names, ", ")))
			}
		}
		if _, err := profileAssign.Call(c.Context(), &pb.ProfileAssignRequest{ProfileId: id}); err != nil {
			return err
		}
	}
	response, err := profileCurrent.Call(c.Context(), &pb.ProfileCurrentRequest{})
	if err != nil {
		return err
	}
	return c.Reply(c.T("agent.profile_current", "name", response.Profile.Name, "sessions", response.Profile.Sessions))
}

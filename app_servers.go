package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"ssxng/internal/api"
	"ssxng/internal/config"
	"ssxng/internal/ssurl"
)

// Server profiles: list/create/update/delete, selection and ordering,
// ss:// import and export, and reachability testing.

func (a *App) serverViews() []api.ServerView {
	servers := a.store.Servers()
	cur, _ := a.store.CurrentServer()
	out := make([]api.ServerView, 0, len(servers))
	for _, s := range servers {
		out = append(out, a.serverView(s, cur.ID))
	}
	return out
}

func (a *App) serverView(s config.ServerProfile, currentID string) api.ServerView {
	running := a.engine != nil && a.engine.Running()
	active := running && s.ID == currentID
	return api.ServerView{
		ID:        s.ID,
		Host:      s.Host,
		Port:      s.Port,
		Method:    s.Method,
		Password:  s.Password,
		Remark:    s.Remark,
		Plugin:    s.Plugin,
		Title:     s.Title(),
		HasPlugin: strings.TrimSpace(s.Plugin) != "",
		Active:    active,
		Connected: active,
	}
}

// AddServer stores a new server.
func (a *App) AddServer(view api.ServerView) (api.ServerView, error) {
	profile := configFromView(view)
	profile.ID = ""
	if err := profile.Validate(); err != nil {
		return api.ServerView{}, err
	}
	profile.ID = config.NewID()
	if err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		return append(list, profile), nil
	}); err != nil {
		return api.ServerView{}, err
	}
	if cur, ok := a.store.CurrentServer(); !ok || cur.ID == "" {
		if err := a.store.SetCurrentServerID(profile.ID); err != nil {
			return api.ServerView{}, err
		}
	}
	a.logf("info", "app", "已添加服务器 %s", profile.Title())
	a.afterServersChanged()
	return a.serverView(profile, profile.ID), nil
}

// UpdateServer changes an existing server.
func (a *App) UpdateServer(view api.ServerView) error {
	if strings.TrimSpace(view.ID) == "" {
		return errors.New("缺少服务器 ID")
	}
	profile := configFromView(view)
	profile.ID = view.ID
	if err := profile.Validate(); err != nil {
		return err
	}
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		for i := range list {
			if list[i].ID == profile.ID {
				list[i] = profile
				return list, nil
			}
		}
		return list, fmt.Errorf("找不到服务器 %s", profile.ID)
	})
	if err != nil {
		return err
	}
	a.logf("info", "app", "已更新服务器 %s", profile.Title())
	a.afterServersChanged()
	return nil
}

// DeleteServer removes a server.
func (a *App) DeleteServer(id string) error {
	var removed config.ServerProfile
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		out := make([]config.ServerProfile, 0, len(list))
		found := false
		for _, s := range list {
			if s.ID == id {
				removed = s
				found = true
				continue
			}
			out = append(out, s)
		}
		if !found {
			return list, fmt.Errorf("找不到服务器 %s", id)
		}
		return out, nil
	})
	if err != nil {
		return err
	}
	a.logf("info", "app", "已删除服务器 %s", removed.Title())

	if cur, ok := a.store.CurrentServer(); !ok || cur.ID == id {
		next := ""
		if servers := a.store.Servers(); len(servers) > 0 {
			next = servers[0].ID
		}
		if err := a.store.SetCurrentServerID(next); err != nil {
			return err
		}
		if next == "" && a.settings().Enabled {
			a.logf("warn", "app", "已没有可用服务器，正在关闭代理")
			if err := a.SetEnabled(false); err != nil {
				return err
			}
		}
	}
	a.afterServersChanged()
	return nil
}

// SelectServer makes a server the active one.
func (a *App) SelectServer(id string) error {
	if _, ok := a.store.ServerByID(id); !ok {
		return fmt.Errorf("找不到服务器 %s", id)
	}
	if err := a.store.SetCurrentServerID(id); err != nil {
		return err
	}
	srv, _ := a.store.ServerByID(id)
	a.logf("info", "app", "已选择服务器 %s", srv.Title())
	a.afterServersChanged()
	return nil
}

// MoveServer moves a server up (delta < 0) or down (delta > 0) in the list.
func (a *App) MoveServer(id string, delta int) error {
	if delta == 0 {
		return nil
	}
	step := 1
	if delta < 0 {
		step = -1
	}
	err := a.store.MutateServers(func(list []config.ServerProfile) ([]config.ServerProfile, error) {
		idx := -1
		for i, s := range list {
			if s.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return list, fmt.Errorf("找不到服务器 %s", id)
		}
		j := idx + step
		if j < 0 || j >= len(list) {
			return list, nil
		}
		list[idx], list[j] = list[j], list[idx]
		return list, nil
	})
	if err != nil {
		return err
	}
	a.afterServersChanged()
	return nil
}

// ImportSSURL parses an ss:// link and adds it.
func (a *App) ImportSSURL(raw string) (api.ServerView, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return api.ServerView{}, errors.New("请输入 ss:// 链接")
	}
	parsed, err := ssurl.Parse(raw)
	if err != nil {
		return api.ServerView{}, err
	}
	method, ok := config.CanonicalMethod(parsed.Method)
	if !ok {
		return api.ServerView{}, fmt.Errorf("不支持的加密方式 %q，本版本支持: %s",
			parsed.Method, strings.Join(config.SupportedMethods, ", "))
	}
	profile := config.ServerProfile{
		Host:          parsed.Host,
		Port:          parsed.Port,
		Method:        method,
		Password:      parsed.Password,
		Remark:        parsed.Remark,
		Plugin:        parsed.Plugin,
		PluginOptions: parsed.PluginOptions,
	}
	if err := profile.Validate(); err != nil {
		return api.ServerView{}, err
	}
	for _, existing := range a.store.Servers() {
		if strings.EqualFold(existing.Host, profile.Host) && existing.Port == profile.Port && existing.Method == profile.Method {
			if err := a.SelectServer(existing.ID); err != nil {
				return api.ServerView{}, err
			}
			a.logf("info", "app", "服务器已存在，直接选中: %s", existing.Title())
			return a.serverView(existing, existing.ID), nil
		}
	}
	view, err := a.AddServer(api.ServerView{
		Host:     profile.Host,
		Port:     profile.Port,
		Method:   profile.Method,
		Password: profile.Password,
		Remark:   profile.Remark,
		Plugin:   profile.Plugin,
	})
	if err != nil {
		return api.ServerView{}, err
	}
	if err := a.SelectServer(view.ID); err != nil {
		return api.ServerView{}, err
	}
	return view, nil
}

// ExportSSURL renders a server as a SIP002 ss:// link.
func (a *App) ExportSSURL(id string) (string, error) {
	srv, ok := a.store.ServerByID(id)
	if !ok {
		return "", fmt.Errorf("找不到服务器 %s", id)
	}
	return ssurl.Profile{
		Host:          srv.Host,
		Port:          srv.Port,
		Method:        srv.Method,
		Password:      srv.Password,
		Remark:        srv.Remark,
		Plugin:        srv.Plugin,
		PluginOptions: srv.PluginOptions,
	}.String(), nil
}

// TestServer checks TCP connectivity to a server.
func (a *App) TestServer(id string) error {
	srv, ok := a.store.ServerByID(id)
	if !ok {
		return fmt.Errorf("找不到服务器 %s", id)
	}
	addr := net.JoinHostPort(srv.Host, strconv.Itoa(srv.Port))
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 6*time.Second)
	if err != nil {
		a.toastError("app", fmt.Errorf("无法连接 %s: %v", addr, err))
		return err
	}
	_ = conn.Close()
	msg := fmt.Sprintf("%s 可以连接（%d ms）", addr, time.Since(start).Milliseconds())
	a.logf("info", "app", "%s", msg)
	a.emitToast("info", msg)
	return nil
}

// afterServersChanged re-points a running engine at the current server.
func (a *App) afterServersChanged() {
	sett := a.settings()
	cur, ok := a.store.CurrentServer()
	if a.engine != nil && a.engine.Running() {
		switch {
		case !ok:
			a.logf("warn", "app", "没有可用服务器，正在关闭代理")
			if err := a.SetEnabled(false); err != nil {
				a.toastError("app", err)
			}
			return
		case sett.Enabled:
			if err := a.engine.SetServer(cur); err != nil {
				a.toastError("proxy", err)
			}
		}
	}
	a.emitState()
}

// ---------------------------------------------------------------- conversion

func configFromView(view api.ServerView) config.ServerProfile {
	method, ok := config.CanonicalMethod(view.Method)
	if !ok {
		method = strings.ToLower(strings.TrimSpace(view.Method))
	}
	return config.ServerProfile{
		ID:       strings.TrimSpace(view.ID),
		Host:     strings.TrimSpace(view.Host),
		Port:     view.Port,
		Method:   method,
		Password: view.Password,
		Remark:   strings.TrimSpace(view.Remark),
		Plugin:   strings.TrimSpace(view.Plugin),
	}
}

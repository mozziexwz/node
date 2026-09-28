package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type reconnectPrompt interface {
	Read(string, bool) (string, error)
	Print(string, ...any)
}
type reconnectCheckpoint struct {
	AgentID     string                `json:"agentId"`
	Host        string                `json:"host"`
	Port        int                   `json:"port"`
	Fingerprint string                `json:"fingerprint"`
	RemoteDir   string                `json:"remoteDir"`
	Request     *RelayRecoveryRequest `json:"request,omitempty"`
	Routes      []string              `json:"routes,omitempty"`
}

// The wizard is still root-local on BOTH machines. It transports the existing
// protected snapshot/adopt protocol over pinned SSH; no new public recovery
// endpoint or bearer-token bypass is introduced.
func runRelayReconnect(databaseURL, masterKey, publicURL, directory string, ui reconnectPrompt) error {
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("恢复向导需要现有 PostgreSQL 站点")
	}
	if !relayRecoveryOrigin(strings.TrimRight(publicURL, "/")) {
		return errors.New("节点重新连接需要有效的 HTTPS 面板域名")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return errors.New("恢复向导私有工作目录不可用")
	}
	aead, err := backupExportCipher(masterKey, "")
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法打开站点数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	a := &App{Store: &Store{db: db, dialect: "postgres"}, aead: aead, Config: Config{PublicURL: strings.TrimRight(publicURL, "/")}}
	ui.Print("节点管理连接恢复向导\n原转发会尽量保留。请先停用旧面板，避免两个面板同时控制节点。\n")
	confirm, err := ui.Read("已确认旧面板停用或隔离？输入 YES 继续：", false)
	if err != nil || confirm != "YES" {
		return errors.New("已取消")
	}
	for {
		var agents []RelayAgent
		verified := map[string]bool{}
		var control RelayV2Control
		err = a.Store.View(func(s *State) error {
			if !boolSetting(s, "maintenance") {
				return errors.New("请先在后台开启维护模式，再运行恢复向导")
			}
			control, _ = LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
			for _, v := range ListDocs[RelayAgent](s, "relay_agents") {
				if v.ReconcileState == "recovery_required" {
					agents = append(agents, v)
					p, _ := LoadDoc[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", v.ID)
					verified[v.ID] = p.AllDecided && p.VerifiedAt > time.Now().Add(-90*time.Second).UnixMilli()
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !control.RecoveryRequired && len(agents) == 0 {
			ui.Print("当前没有需要恢复管理连接的节点。日常升级请使用后台节点维护入口。\n")
			return nil
		}
		sort.Slice(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })
		for i, v := range agents {
			status := "待连接"
			if verified[v.ID] {
				status = "连接已核对"
			}
			ui.Print("%d) %s [%s] %s\n", i+1, v.Name, v.Address, status)
		}
		ui.Print("0) 完成全站恢复核对\nq) 保存进度并退出\n")
		choice, e := ui.Read("请选择：", false)
		if e != nil {
			return e
		}
		if choice == "q" {
			ui.Print("进度已保留，下次运行相同命令继续。\n")
			return nil
		}
		if choice == "0" {
			answer, e := ui.Read("已核对备份之后的收款、权益和流量？输入 YES 完成节点核对（网站仍保持维护）：", false)
			if e != nil || answer != "YES" {
				continue
			}
			if e = a.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+control.Epoch, time.Now().UnixMilli()); e != nil {
				ui.Print("尚不能完成：仍有节点未确认当前配置，或存在待处理的线路差异。请先完成上方节点；后台可查看对应线路状态。\n")
				continue
			}
			ui.Print("全部节点已完成核对。请回后台验收线路，再关闭维护并按需开启兑换与支付。\n")
			return nil
		}
		n, e := strconv.Atoi(choice)
		if e != nil || n < 1 || n > len(agents) {
			ui.Print("请选择列表中的编号。\n")
			continue
		}
		if verified[agents[n-1].ID] {
			ui.Print("此节点已经核对，可继续其他节点或选择 0 完成。\n")
			continue
		}
		if e = a.reconnectOne(agents[n-1], directory, ui); e != nil {
			ui.Print("处理未完成：%s\n恢复材料已保留，下次选择同一节点继续。\n", e.Error())
		}
	}
}

func saveReconnectCheckpoint(file string, cp reconnectCheckpoint) error {
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".reconnect-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), file)
}

func reconnectSelections(report RelayRecoveryReport, choose func(RelayRecoveryDifference) (string, error)) ([]RelayRecoverySelection, error) {
	if len(report.MissingOnNode) > 0 {
		return nil, errors.New("面板记录的部分转发不在此节点中；需先核对节点磁盘或原节点，不能推定这些转发已经停止")
	}
	out := []RelayRecoverySelection{}
	for _, row := range report.Differences {
		action := "adopt"
		if !row.CanAdopt {
			var err error
			action, err = choose(row)
			if err != nil {
				return nil, err
			}
			if action != "stop" {
				return nil, errors.New("已保留差异转发，请核对后再继续")
			}
		}
		out = append(out, RelayRecoverySelection{RuleID: row.RuleID, Action: action})
	}
	return out, nil
}

func (a *App) reconnectOne(agent RelayAgent, directory string, ui reconnectPrompt) error {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(agent.ID) {
		return errRelayRecovery
	}
	file := filepath.Join(directory, "reconnect-"+agent.ID+".json")
	cp := reconnectCheckpoint{AgentID: agent.ID, Host: agent.Address, Port: 22}
	if raw, e := readRegularLimited(file, 32<<20); e == nil {
		if !backupPauseDecode(raw, &cp) || cp.AgentID != agent.ID {
			return errors.New("原恢复记录无效，请保留记录检查")
		}
	} else if _, e = os.Lstat(file); !os.IsNotExist(e) {
		return errors.New("原恢复记录不可读取")
	}
	if cp.RemoteDir == "" {
		host, err := ui.Read("节点公网 IP ["+cp.Host+"]：", false)
		if err != nil {
			return err
		}
		if host != "" {
			cp.Host = host
		}
		port, err := ui.Read("节点 SSH 端口 [22]：", false)
		if err != nil {
			return err
		}
		if port != "" {
			cp.Port, err = strconv.Atoi(port)
			if err != nil {
				return errors.New("SSH 端口无效")
			}
		}
		ui.Print("请从该节点可信控制台复制指纹：ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\n")
		cp.Fingerprint, err = ui.Read("节点 SHA256 指纹：", false)
		if err != nil {
			return err
		}
	}
	if executor.PublicIP(cp.Host) != nil || cp.Port < 1 || cp.Port > 65535 || !strings.HasPrefix(cp.Fingerprint, "SHA256:") {
		return errors.New("节点 IP、端口或指纹无效")
	}
	password, err := ui.Read("节点 root SSH 密码（不回显）：", true)
	if err != nil {
		return err
	}
	address := net.JoinHostPort(cp.Host, strconv.Itoa(cp.Port))
	conn, err := net.DialTimeout("tcp", address, 12*time.Second)
	if err != nil {
		return errors.New("无法连接节点 SSH，请检查 IP、端口和网络")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	cc, ch, req, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.Password(password)}, HostKeyCallback: executor.PinnedHostKey(cp.Fingerprint), HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Timeout: 12 * time.Second})
	password = ""
	if err != nil {
		return errors.New("SSH 认证未通过，请核对节点指纹和 root 密码")
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, ch, req)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	sf, err := sftp.NewClient(client)
	if err != nil {
		return errors.New("节点 SFTP 不可用")
	}
	defer sf.Close()
	if cp.RemoteDir == "" {
		out, e := reconnectCommand(client, "umask 077; mktemp -d /root/msboost-reconnect.XXXXXXXX")
		if e != nil {
			return e
		}
		cp.RemoteDir = strings.TrimSpace(string(out))
		if !regexp.MustCompile(`^/root/msboost-reconnect\.[A-Za-z0-9]{8}$`).MatchString(cp.RemoteDir) {
			return errors.New("节点未返回有效私有工作目录")
		}
		if e = saveReconnectCheckpoint(file, cp); e != nil {
			return errors.New("无法保存恢复进度")
		}
	}
	if !regexp.MustCompile(`^/root/msboost-reconnect\.[A-Za-z0-9]{8}$`).MatchString(cp.RemoteDir) {
		return errors.New("恢复进度中的节点目录无效")
	}
	info, err := sf.Lstat(cp.RemoteDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("节点私有工作目录缺失或权限异常")
	}
	uid, err := executor.SFTPFileUID(info)
	if err != nil || uid != 0 {
		return errors.New("节点工作目录不是 root 所有")
	}
	if cp.Request == nil {
		ui.Print("正在核对节点身份和正在运行的转发…\n")
		if _, checkErr := sf.Lstat(cp.RemoteDir + "/snapshot.json"); os.IsNotExist(checkErr) {
			if _, err = reconnectCommand(client, "/usr/local/bin/msboost-agent --capability relay-recovery --state-dir /var/lib/msboost-relay --recovery-action snapshot --recovery-file "+cp.RemoteDir+"/snapshot.json"); err != nil {
				return errors.New("无法读取节点状态，请确认节点服务已运行且使用新版程序")
			}
		} else if checkErr != nil {
			return errors.New("无法检查节点原恢复快照，请检查连接")
		}
		raw, err := readReconnectRemote(sf, cp.RemoteDir+"/snapshot.json")
		if err != nil {
			return err
		}
		var snapshot relayruntime.V2RecoverySnapshot
		if !backupPauseDecode(raw, &snapshot) || snapshot.AgentID != agent.ID {
			return errors.New("此 VPS 的节点身份与所选节点不同，未重新绑定")
		}
		var report RelayRecoveryReport
		if err = a.Store.View(func(s *State) error {
			var e error
			report, e = a.relayRecoveryReport(s, snapshot)
			if e != nil {
				return e
			}
			// Store.View loads a detached transaction snapshot. Restore only
			// previously enabled routes in this in-memory policy simulation.
			for _, route := range ListDocs[Route](s, "routes") {
				prior, _ := LoadDoc[map[string]bool](s, "relay_recovery_route_states", route.ID)
				route.Enabled = route.Enabled || prior["enabled"]
				_ = SaveDoc(s, "routes", route.ID, route)
			}
			checkAgent := agent
			checkAgent.Enabled = true
			for i := range report.Differences {
				d := &report.Differences[i]
				cmd, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", agent.ID+":"+d.RuleID)
				if !d.CanAdopt || cmd.Action != "upsert" {
					continue
				}
				found := false
				for _, rule := range ListDocs[UserRule](s, "user_rules") {
					if rule.ID != d.RuleID {
						continue
					}
					found = true
					action, _ := relayV2Desired(s, checkAgent, rule, time.Now().UnixMilli())
					if action != "upsert" {
						d.CanAdopt = false
						d.Difference = "current_entitlement_or_route_disallows_forwarding"
					} else {
						cp.Routes = append(cp.Routes, rule.RouteID)
					}
				}
				if !found {
					d.CanAdopt = false
				}
			}
			return nil
		}); err != nil {
			return errRelayRecovery
		}
		decisions, err := reconnectSelections(report, func(row RelayRecoveryDifference) (string, error) {
			ui.Print("转发 %s 的配置或当前使用资格不符，不能直接恢复。保留现状请选择退出；停止这条转发会断开其连接。\n", row.RuleID)
			answer, e := ui.Read("明确停止这条差异转发？输入 STOP，其余退出：", false)
			if e != nil {
				return "", e
			}
			if answer == "STOP" {
				return "stop", nil
			}
			return "hold", nil
		})
		if err != nil {
			return err
		}
		ui.Print("已核对 %d 条转发，配置一致的将保留运行。\n", len(decisions))
		answer, err := ui.Read("确认连接此节点，并重新启用备份前已启用且资格仍有效的线路？输入 YES：", false)
		if err != nil {
			return err
		}
		if answer != "YES" {
			return errors.New("已取消，可下次继续")
		}
		cp.Request = &RelayRecoveryRequest{Snapshot: snapshot, Fingerprint: report.Fingerprint, Decisions: decisions, Confirmation: "OLD_CONTROL_ISOLATED " + snapshot.RecoveryID}
		if err = saveReconnectCheckpoint(file, cp); err != nil {
			return errors.New("无法保存本次确认，未提交恢复")
		}
	}
	if err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[RelayAgent](s, "relay_agents", agent.ID)
		if !ok || !boolSetting(s, "maintenance") || v.ReconcileState != "recovery_required" {
			return errRelayRecovery
		}
		v.Enabled = true
		for _, id := range cp.Routes {
			route, ok := LoadDoc[Route](s, "routes", id)
			prior, _ := LoadDoc[map[string]bool](s, "relay_recovery_route_states", id)
			if !ok || !route.Enabled && !prior["enabled"] {
				return errRelayRecovery
			}
			route.Enabled = true
			if e := SaveDoc(s, "routes", id, route); e != nil {
				return e
			}
		}
		return SaveDoc(s, "relay_agents", v.ID, v)
	}); err != nil {
		return err
	}
	plan, err := a.prepareRelayRecovery(*cp.Request, time.Now().UnixMilli())
	if err != nil {
		return errRelayRecovery
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	defer clear(raw)
	planPath := cp.RemoteDir + "/plan.json"
	if err = writeReconnectRemote(sf, planPath, raw); err != nil {
		return err
	}
	if _, err = reconnectCommand(client, "/usr/local/bin/msboost-agent --capability relay-recovery --state-dir /var/lib/msboost-relay --recovery-action adopt --recovery-file "+planPath); err != nil {
		return errors.New("节点尚未确认恢复操作，原请求已保存，请重新运行向导继续同一节点")
	}
	ui.Print("材料已自动传送，等待节点确认管理连接…\n")
	for i := 0; i < 30; i++ {
		confirmed := false
		err = a.Store.View(func(s *State) error {
			p, _ := LoadDoc[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", agent.ID)
			confirmed = p.PlanID == plan.PlanID && p.AllDecided && p.VerifiedAt > time.Now().Add(-90*time.Second).UnixMilli()
			return nil
		})
		if err != nil {
			return err
		}
		if confirmed {
			ui.Print("节点管理连接已核对，原服务没有整体重启。请继续其他节点，最后选择 0 完成全站核对。\n")
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return errors.New("节点尚未上报确认，请检查节点到面板的 HTTPS 连接，然后继续此向导")
}

func reconnectCommand(client *ssh.Client, command string) ([]byte, error) {
	s, err := client.NewSession()
	if err != nil {
		return nil, errors.New("无法建立节点命令会话")
	}
	defer s.Close()
	timer := time.AfterFunc(30*time.Second, func() { _ = s.Close() })
	defer timer.Stop()
	var out bytes.Buffer
	s.Stdout = &limitedReconnectWriter{Writer: &out, Left: 1 << 20}
	s.Stderr = io.Discard
	if err = s.Run(command); err != nil {
		return nil, errors.New("节点操作未确认完成")
	}
	return out.Bytes(), nil
}

type limitedReconnectWriter struct {
	io.Writer
	Left int
}

func (w *limitedReconnectWriter) Write(p []byte) (int, error) {
	if len(p) > w.Left {
		return 0, errors.New("节点输出超出限制")
	}
	w.Left -= len(p)
	return w.Writer.Write(p)
}
func readReconnectRemote(sf *sftp.Client, name string) ([]byte, error) {
	i, e := sf.Lstat(name)
	if e != nil || !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || i.Size() > 32<<20 {
		return nil, errors.New("节点恢复文件缺失或权限异常")
	}
	u, e := executor.SFTPFileUID(i)
	if e != nil || u != 0 {
		return nil, errors.New("节点恢复文件身份不符")
	}
	f, e := sf.Open(name)
	if e != nil {
		return nil, errors.New("不能读取节点恢复文件")
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, 32<<20+1))
	if e != nil || len(raw) > 32<<20 {
		return nil, errors.New("节点恢复文件读取失败")
	}
	return raw, nil
}
func writeReconnectRemote(sf *sftp.Client, name string, raw []byte) error {
	if _, e := sf.Lstat(name); e == nil {
		prior, e := readReconnectRemote(sf, name)
		defer clear(prior)
		if e != nil || !bytes.Equal(prior, raw) {
			return errors.New("节点已有另一份恢复计划，请保留记录核对")
		}
		return nil
	} else if !os.IsNotExist(e) {
		return errors.New("无法检查节点恢复计划")
	}
	var suffix [12]byte
	if _, e := rand.Read(suffix[:]); e != nil {
		return e
	}
	temp := name + ".partial-" + hex.EncodeToString(suffix[:])
	f, e := sf.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if e != nil {
		return errors.New("不能创建节点私有恢复计划")
	}
	defer f.Close()
	defer sf.Remove(temp)
	if e = f.Chmod(0600); e != nil {
		return errors.New("不能设置恢复计划私有权限")
	}
	if _, e = f.Write(raw); e != nil {
		return errors.New("恢复计划传送未完成")
	}
	if e = f.Close(); e != nil {
		return errors.New("恢复计划保存未确认")
	}
	actual, e := readReconnectRemote(sf, temp)
	defer clear(actual)
	if e != nil || !bytes.Equal(actual, raw) {
		return errors.New("恢复计划回读校验失败")
	}
	if e = sf.Rename(temp, name); e != nil {
		return errors.New("恢复计划提交未确认，请重试")
	}
	return nil
}

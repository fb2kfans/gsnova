package proxy

import (
	"bufio"
	"bytes"
	"common"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"
	"util"
)

var spac_script_path []string

type JsonRule struct {
	Method       []string
	Host         []string
	URL          []string
	Proxy        []string
	Filter       []string
	Protocol     string
	Attr         []string
	method_regex []*regexp.Regexp
	host_regex   []*regexp.Regexp
	url_regex    []*regexp.Regexp
}

func loadSpacScript() error {
	rules := []*JsonRule{}
	var err error
	defer func() {
		if nil != err {
			log.Printf("Failed to init SPAC for reason:%v", err)
		}
	}()
	for _, path := range spac_script_path {
		file, e := ioutil.ReadFile(path)
		err = e
		if err == nil {
			tmp := []*JsonRule{}
			err = json.Unmarshal(file, &tmp)
			if nil != err {
				log.Printf("[ERROR]Failed to unmarshal spac script file:%s for reason:%v\n", path, err)
				continue
			}
			for _, json_rule := range tmp {
				err = json_rule.init()
				if nil != err {
					return err
				}
			}
			rules = append(rules, tmp...)
		} else {
			return err
		}
	}
	spac.rules = rules
	return nil
}

func reloadSpacScript() {
	tick := time.NewTicker(5 * time.Second)
	mod_times := make([]time.Time, len(spac_script_path))
	for {
		select {
		case <-tick.C:
			modified := false
			for i, path := range spac_script_path {
				f, err := os.Stat(path)
				if nil == err {
					if !mod_times[i].IsZero() && mod_times[i].Before(f.ModTime()) {
						modified = true
					}
					mod_times[i] = f.ModTime()
				}
			}
			if modified {
				loadSpacScript()
			}
		}
	}
}

func matchRegexs(str string, rules []*regexp.Regexp) bool {
	if len(rules) == 0 {
		return true
	}
	for _, regex := range rules {
		if regex.MatchString(str) {
			return true
		}
	}

	return false
}

func initRegexSlice(rules []string) ([]*regexp.Regexp, error) {
	regexs := make([]*regexp.Regexp, 0)
	for _, originrule := range rules {
		reg, err := util.PrepareRegexp(originrule, true)
		if nil != err {
			log.Printf("Invalid pattern:%s for reason:%v\n", originrule, err)
			return nil, err
		} else {
			regexs = append(regexs, reg)
		}
	}

	return regexs, nil
}

func (r *JsonRule) init() (err error) {
	r.method_regex, err = initRegexSlice(r.Method)
	if nil != err {
		return
	}
	r.host_regex, err = initRegexSlice(r.Host)
	if nil != err {
		return
	}
	r.url_regex, err = initRegexSlice(r.URL)

	return
}

func (r *JsonRule) matchProtocol(req *http.Request, isHttpsConn bool) bool {
	if len(r.Protocol) > 0 {
		protocol := "http"
		if strings.EqualFold(req.Method, "Connect") || isHttpsConn {
			protocol = "https"
		}
		return strings.EqualFold(r.Protocol, protocol)
	}
	return true
}

func (r *JsonRule) matchFilters(req *http.Request) bool {
	matched := true
	for _, filter := range r.Filter {
		matched = matched && invokeFilter(filter, req)
		if !matched {
			return false
		}
	}
	return matched
}

func (r *JsonRule) match(req *http.Request, isHttpsConn bool) bool {
	return r.matchFilters(req) && r.matchProtocol(req, isHttpsConn) && matchRegexs(req.Method, r.method_regex) && matchRegexs(req.Host, r.host_regex) && matchRegexs(req.RequestURI, r.url_regex)
}

type SpacConfig struct {
	defaultRule string
	rules       []*JsonRule
}

var spac *SpacConfig

var registedRemoteConnManager map[string]RemoteConnectionManager = make(map[string]RemoteConnectionManager)

func RegisteRemoteConnManager(connManager RemoteConnectionManager) {
	registedRemoteConnManager[connManager.GetName()] = connManager
}

var pac_proxy = "127.0.0.1:48100"

var pacGenFormatter = `/*
 * Proxy Auto-Config file generated by autoproxy2pac
 *  Rule source: {{.RuleListUrl}}
 *  Last update: {{.RuleListDate}}
 */
function FindProxyForURL(url, host) {
	var {{.ProxyVar}} = "{{.ProxyString}}";
	var {{.DefaultVar}} = "{{.DefaultString}}";
	{{.CustomCodePre}}
	{{.RulesBegin}}
	{{.RuleListCode}}
	{{.RulesEnd}}
	{{.CustomCodePost}}
	return {{.DefaultVar}};
}`

func load_gfwlist_rule() {
	var buffer bytes.Buffer
	if content, err := ioutil.ReadFile(common.Home + "spac/snova-gfwlist.txt"); nil == err {
		buffer.Write(content)
	}
	buffer.WriteString("\n")
	if content, err := ioutil.ReadFile(common.Home + "spac/user-gfwlist.txt"); nil == err {
		buffer.Write(content)
	}
	init_gfwlist_func(buffer.String())
}

func generatePAC(url, date, content string) string {
	// Prepare some data to insert into the template.
	type PACContent struct {
		RuleListUrl, RuleListDate     string
		ProxyVar, ProxyString         string
		DefaultVar, DefaultString     string
		CustomCodePre, CustomCodePost string
		RulesBegin, RulesEnd          string
		RuleListCode                  string
	}
	var pac = &PACContent{}
	pac.RulesBegin = "//-- AUTO-GENERATED RULES, DO NOT MODIFY!"
	pac.RulesEnd = "//-- END OF AUTO-GENERATED RULES"
	pac.ProxyVar = "PROXY"
	pac.RuleListUrl = url
	pac.RuleListDate = date
	pac.ProxyString = "PROXY " + pac_proxy
	pac.DefaultVar = "DEFAULT"
	pac.DefaultString = "DIRECT"
	jscode := []string{}

	if usercontent, err := ioutil.ReadFile(common.Home + "spac/user-gfwlist.txt"); nil == err {
		content = content + "\n" + string(usercontent)
	}

	reader := bufio.NewReader(strings.NewReader(content))
	i := 0
	for {
		line, _, err := reader.ReadLine()
		if nil != err {
			break
		}
		//from the second line
		i = i + 1
		if i == 1 {
			continue
		}
		str := string(line)
		str = strings.TrimSpace(str)

		proxyVar := "PROXY"
		//comment
		if strings.HasPrefix(str, "!") || len(str) == 0 {
			continue
		}
		if strings.HasPrefix(str, "@@") {
			str = str[2:]
			proxyVar = "DEFAULT"
		}
		jsRegexp := ""

		if strings.HasPrefix(str, "/") && strings.HasSuffix(str, "/") {
			jsRegexp = str[1 : len(str)-1]
		} else {
			if tmp, err := regexp.Compile("\\*+"); err == nil {
				jsRegexp = tmp.ReplaceAllString(str, "*")
			}

			if tmp, err := regexp.Compile("\\^\\|$"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "^", tmp, 1)
			}
			if tmp, err := regexp.Compile("(\\W)"); err == nil {
				jsRegexp = tmp.ReplaceAllString(jsRegexp, "\\$0")
			}
			jsRegexp = strings.Replace(jsRegexp, "\\*", ".*", -1)

			if tmp, err := regexp.Compile("\\\\^"); err == nil {
				jsRegexp = tmp.ReplaceAllString(jsRegexp, "(?:[^\\w\\-.%\u0080-\uFFFF]|$)")
			}

			if tmp, err := regexp.Compile("^\\\\\\|\\\\\\|"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "^[\\w\\-]+:\\/+(?!\\/)(?:[^\\/]+\\.)?", tmp, 1)
			}
			if tmp, err := regexp.Compile("^\\\\\\|"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "^", tmp, 1)
			}
			if tmp, err := regexp.Compile("\\\\\\|$"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "$", tmp, 1)
			}
			if tmp, err := regexp.Compile("^(\\.\\*)"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "", tmp, 1)
			}
			if tmp, err := regexp.Compile("(\\.\\*)$"); err == nil {
				jsRegexp = util.RegexpReplace(jsRegexp, "", tmp, 1)
			}
			if len(jsRegexp) == 0 {
				log.Printf("There is one rule that matches all URL, which is highly *NOT* recommended: %s\n", str)
			}
		}
		jsLine := fmt.Sprintf("if(/%s/i.test(url)) return %s;", jsRegexp, proxyVar)
		if proxyVar == "DEFAULT" {
			//log.Printf("%s\n", jsLine)
			jscode = append(jscode[:0], append([]string{jsLine}, jscode[0:]...)...)
		} else {
			jscode = append(jscode, jsLine)
		}
	}
	pac.RuleListCode = strings.Join(jscode, "\r\n\t")
	t := template.Must(template.New("pacGenFormatter").Parse(pacGenFormatter))
	var buffer bytes.Buffer
	err := t.Execute(&buffer, pac)
	if err != nil {
		log.Println("Executing template:", err)
	}
	return buffer.String()
}

func fetchCloudSpacScript(url string) {
	time.Sleep(5 * time.Second)
	log.Printf("Fetch remote clound spac rule:%s\n", url)
	var file_ts time.Time
	if fi, err := os.Stat(spac_script_path[1]); nil == err {
		file_ts = fi.ModTime()
	}

	body, _, err := util.FetchLateastContent(url, common.ProxyPort, file_ts, false)

	if nil == err && len(body) > 0 {
		ioutil.WriteFile(spac_script_path[1], body, 0666)
	}
	if nil != err {
		log.Printf("Failed to fetch spac cloud script for reason:%v\n", err)
	}
}

func generatePACFromGFWList(url string) {
	time.Sleep(5 * time.Second)
	log.Printf("Generate PAC from  gfwlist %s\n", url)
	load_gfwlist_rule()
	gfwlist_txt := common.Home+"spac/snova-gfwlist.txt"
	var file_ts time.Time
	if fi, err := os.Stat(gfwlist_txt); nil == err {
		file_ts = fi.ModTime()
	}
	body, last_mod_date, err := util.FetchLateastContent(url, common.ProxyPort, file_ts, false)
	if nil == err && len(body) > 0 {
		content, _ := base64.StdEncoding.DecodeString(string(body))
		ioutil.WriteFile(gfwlist_txt, content, 0666)
		hf := common.Home + "spac/snova-gfwlist.pac"
		file_content := generatePAC(url, last_mod_date, string(content))
		ioutil.WriteFile(hf, []byte(file_content), 0666)
		load_gfwlist_rule()
	}
	if nil != err {
		log.Printf("Failed to fetch gfwlist for reason:%v\n", err)
	}
}

func PostInitSpac() {
	if spac.defaultRule == AUTO_NAME {
		if gae_enable {
			spac.defaultRule = GAE_NAME
		} else if c4_enable {
			spac.defaultRule = C4_NAME
		} else if ssh_enable {
			spac.defaultRule = SSH_NAME
		} else {
			spac.defaultRule = DIRECT_NAME
		}
	}
}

func InitSpac() {
	spac = &SpacConfig{}
	os.Mkdir(common.Home+"spac/", 0755)
	spac.defaultRule, _ = common.Cfg.GetProperty("SPAC", "Default")
	if len(spac.defaultRule) == 0 {
		spac.defaultRule = GAE_NAME
	}

	spac.rules = make([]*JsonRule, 0)
	if enable, exist := common.Cfg.GetIntProperty("SPAC", "Enable"); exist {
		if enable == 0 {
			return
		}
	}
	if url, exist := common.Cfg.GetProperty("SPAC", "GFWList"); exist {
		go generatePACFromGFWList(url)
	}

	if addr, exist := common.Cfg.GetProperty("SPAC", "PACProxy"); exist {
		pac_proxy = addr
	}

	if addr, exist := common.Cfg.GetProperty("SPAC", "CloudRule"); exist {
		go fetchCloudSpacScript(addr)
	}

	//user script has higher priority
	spac_script_path = []string{common.Home + "spac/user_spac.json", common.Home + "spac/cloud_spac.json"}
	loadSpacScript()
	go reloadSpacScript()
	init_spac_func()
}

func selectProxyByRequest(req *http.Request, host, port string, isHttpsConn bool, proxyNames []string) ([]string, map[string]string) {
	attrs := make(map[string]string)
	for _, r := range spac.rules {
		if r.match(req, isHttpsConn) {
			for _, v := range r.Attr {
				attrs[v] = v
			}
			return r.Proxy, attrs
		}
	}

	//	if !isHttpsConn && needInjectCRLF(req.Host) {
	//		return []string{DIRECT_NAME, spac.defaultRule}
	//	}

	if hostsEnable != HOSTS_DISABLE {
		if _, exist := lookupReachableMappingHost(req, net.JoinHostPort(host, port)); exist {
			if !strings.EqualFold(req.Method, "Connect") {
				attrs["CRLF"] = "CRLF"
			}
			return []string{DIRECT_NAME, spac.defaultRule}, attrs
		} else {
			//log.Printf("[WARN]No available IP for %s\n", host)
		}
	}
	return proxyNames, attrs
}

func SelectProxy(req *http.Request, conn net.Conn, isHttpsConn bool) ([]RemoteConnectionManager, map[string]string) {
	host := req.Host
	port := "80"
	if v, p, err := net.SplitHostPort(req.Host); nil == err {
		host = v
		port = p
	}
	proxyNames := []string{spac.defaultRule}
	proxyManagers := make([]RemoteConnectionManager, 0)
	attrs := make(map[string]string)
	need_select_proxy := true
	if util.IsPrivateIP(host) {
		need_select_proxy = false
		proxyNames = []string{DIRECT_NAME}
		if host == "127.0.0.1" || host == util.GetLocalIP() || strings.EqualFold(host, "localhost") {
			if port == common.ProxyPort {
				handleSelfHttpRequest(req, conn)
				return nil, nil
			}
		}
	}

	if need_select_proxy {
		proxyNames, attrs = selectProxyByRequest(req, host, port, isHttpsConn, proxyNames)
	}

	if !isHttpsConn && containsAttr(attrs, ATTR_REDIRECT_HTTPS) {
		redirectHttps(conn, req)
		return nil, nil
	}
	for _, proxyName := range proxyNames {
		if strings.EqualFold(proxyName, DEFAULT_NAME) {
			proxyName = spac.defaultRule
		}
		switch proxyName {
		case GAE_NAME, C4_NAME, SSH_NAME:
			if v, ok := registedRemoteConnManager[proxyName]; ok {
				proxyManagers = append(proxyManagers, v)
			} else {
				log.Printf("No proxy:%s defined for %s\n", proxyName, host)
			}
		case GOOGLE_NAME, GOOGLE_HTTP_NAME:
			if google_enable {
				proxyManagers = append(proxyManagers, httpGoogleManager)
			}
		case GOOGLE_HTTPS_NAME:
			if google_enable {
				proxyManagers = append(proxyManagers, httpsGoogleManager)
			}
		case DIRECT_NAME:
			forward := &Forward{overProxy: false}
			forward.target = req.Host
			if !strings.Contains(forward.target, ":") {
				forward.target = forward.target + ":80"
			}
			if !strings.Contains(forward.target, "://") {
				forward.target = "http://" + forward.target
			}
			proxyManagers = append(proxyManagers, forward)
		default:
			forward := &Forward{overProxy: true}
			forward.target = strings.TrimSpace(proxyName)
			if !strings.Contains(forward.target, "://") {
				forward.target = "http://" + forward.target
			}
			proxyManagers = append(proxyManagers, forward)
		}
	}

	return proxyManagers, attrs
}

package notifications

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Senders for the shoutrrr-style URL types. Each follows the URL format and
// option names of shoutrrr v0.8.0 (containrrr/shoutrrr pkg/services) so a URL
// written for shoutrrr works here, with these differences: unknown options
// are refused, Matrix needs rooms, teams:// takes a Workflows address, and
// generic:// always sends a JSON object. The comments on each type say why.
//
// Every request goes through do(), so they all share the notification
// client and its redirect rule, and run under the notification's ten-second
// deadline.

const (
	discordAPI    = "https://discord.com/api/webhooks/"
	slackHooksAPI = "https://hooks.slack.com/services/"
	slackPostAPI  = "https://slack.com/api/chat.postMessage"
	telegramAPI   = "https://api.telegram.org/bot"
	pushoverAPI   = "https://api.pushover.net/1/messages.json"
	pushbulletAPI = "https://api.pushbullet.com/v2/pushes"
)

func jsonHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}}
}

func doJSON(ctx context.Context, method, target string, header http.Header, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, errDeliveryFailed
	}
	if header == nil {
		header = jsonHeader()
	}
	return do(ctx, request{method: method, url: target, header: header, body: body})
}

// eachDestination runs send for each of n chats, rooms or targets and keeps
// going after a failure. With more than one destination each error names
// its position ("chat 2: HTTP 400 Bad Request").
func eachDestination(label string, n int, send func(i int) error) error {
	var failures []error
	for i := 0; i < n; i++ {
		if err := send(i); err != nil {
			if n > 1 {
				err = fmt.Errorf("%s %d: %w", label, i+1, err)
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// pathSegments splits a URL path into its non-empty parts.
func pathSegments(path string) []string {
	var segments []string
	for _, segment := range strings.Split(path, "/") {
		if segment != "" {
			segments = append(segments, segment)
		}
	}
	return segments
}

func password(user *url.Userinfo) string {
	if user == nil {
		return ""
	}
	value, _ := user.Password()
	return value
}

func username(user *url.Userinfo) string {
	if user == nil {
		return ""
	}
	return user.Username()
}

// ntfy: ntfy://[user:password@][host]/topic. The host defaults to ntfy.sh. The
// message is the request body; everything else goes in ntfy's headers.

type ntfySender struct {
	endpoint string
	user     *url.Userinfo
	title    string
	priority string
	click    string
	headers  http.Header
}

var ntfyPriorities = map[string]string{
	"1": "1", "2": "2", "3": "3", "4": "4", "5": "5",
	"min": "1", "low": "2", "default": "3", "high": "4", "max": "5", "urgent": "5",
}

func parseNtfy(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title", "priority", "tags", "actions", "click", "attach",
		"filename", "delay|at|in", "email", "icon", "cache", "firebase", "scheme")
	if err != nil {
		return nil, err
	}
	topic := strings.Trim(u.Path, "/")
	if topic == "" {
		return nil, errors.New("missing topic")
	}
	host := u.Host
	if host == "" {
		host = "ntfy.sh"
	}
	scheme := strings.ToLower(values["scheme"])
	if scheme == "" {
		scheme = "https"
	} else if scheme != "https" && scheme != "http" {
		return nil, invalidOption("scheme")
	}
	s := ntfySender{
		endpoint: (&url.URL{Scheme: scheme, Host: host, Path: "/" + topic}).String(),
		title:    values["title"],
		click:    values["click"],
		headers:  http.Header{},
	}
	if value, ok := values["priority"]; ok {
		if s.priority, ok = ntfyPriorities[strings.ToLower(value)]; !ok {
			return nil, invalidOption("priority")
		}
	}
	for option, header := range map[string]string{
		"tags": "Tags", "actions": "Actions", "attach": "Attach", "filename": "Filename",
		"delay": "Delay", "email": "Email", "icon": "Icon",
	} {
		if value, ok := values[option]; ok {
			s.headers.Set(header, ntfyHeaderValue(value))
		}
	}
	for option, header := range map[string]string{"cache": "Cache", "firebase": "Firebase"} {
		enabled, err := boolOption(values, option, true)
		if err != nil {
			return nil, err
		}
		if !enabled {
			s.headers.Set(header, "no")
		}
	}
	// As in shoutrrr, a password turns on basic auth. ntfy also takes an
	// access token as the password with an empty user name (ntfy://:tk_...@host).
	if u.User != nil {
		if _, ok := u.User.Password(); !ok {
			return nil, errors.New("missing password")
		}
		s.user = u.User
	}
	return s, nil
}

// ntfyHeaderValue encodes non-ASCII text (a translated title, for example) as
// an RFC 2047 word, which ntfy decodes. Raw UTF-8 in a header isn't reliable.
func ntfyHeaderValue(value string) string {
	return mime.BEncoding.Encode("utf-8", value)
}

func (s ntfySender) send(ctx context.Context, event Event) error {
	header := http.Header{"Content-Type": {"text/plain; charset=utf-8"}}
	for name, values := range s.headers {
		header[name] = values
	}
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	header.Set("Title", ntfyHeaderValue(title))
	priority := s.priority
	if priority == "" {
		priority = map[string]string{"error": "4", "warning": "3", "success": "3"}[event.Severity]
	}
	header.Set("Priority", priority)
	click := s.click
	if click == "" {
		click = event.TimelineURL
	}
	if click != "" {
		header.Set("Click", ntfyHeaderValue(click))
	}
	// ntfy turns a body over 4096 bytes into an attachment, so cut before that.
	_, err := do(ctx, request{url: s.endpoint, header: header, body: []byte(cutBytes(event.Message, 4096)), user: s.user})
	return err
}

// gotify: gotify://host[:port][/subpath]/token. The token goes in the
// X-Gotify-Key header instead of shoutrrr's ?token= query, so it doesn't end
// up in proxy or server access logs.

type gotifySender struct {
	endpoint string
	token    string
	title    string
	priority *int
}

func parseGotify(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title", "priority", "disabletls")
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	path := strings.TrimSuffix(u.Path, "/")
	cut := strings.LastIndex(path, "/")
	token := path[cut+1:]
	if token == "" {
		return nil, errors.New("missing token")
	}
	subpath := ""
	if cut > 0 {
		subpath = path[:cut]
	}
	plain, err := boolOption(values, "disabletls", false)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if plain {
		scheme = "http"
	}
	s := gotifySender{
		endpoint: (&url.URL{Scheme: scheme, Host: u.Host, Path: subpath + "/message"}).String(),
		token:    token,
		title:    values["title"],
	}
	if value, ok := values["priority"]; ok {
		priority, err := strconv.Atoi(value)
		if err != nil {
			return nil, invalidOption("priority")
		}
		s.priority = &priority
	}
	return s, nil
}

func (s gotifySender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	priority := map[string]int{"error": 8, "warning": 5, "success": 2}[event.Severity]
	if s.priority != nil {
		priority = *s.priority
	}
	header := jsonHeader()
	header.Set("X-Gotify-Key", s.token)
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, header, map[string]any{
		"title": title, "message": event.Message, "priority": priority,
	})
	return err
}

// discord: discord://token@webhookid. One embed per notification. shoutrrr's
// splitlines and json (/raw) modes are refused: they send several messages or
// a raw body, and Replicaro sends one embed. thread_id isn't in shoutrrr
// v0.8.0; it's Discord's own query parameter for posting into a thread.

type discordSender struct {
	endpoint string
	title    string
	username string
	avatar   string
	color    *int
	// colorerror, colorwarn and colorinfo by event severity. colordebug is
	// accepted for shoutrrr URLs but has nothing to apply to.
	levelColors map[string]int
}

func parseDiscord(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title", "username", "avatar|avatarurl", "color",
		"colorerror", "colorwarn", "colorinfo", "colordebug", "thread_id")
	if err != nil {
		return nil, err
	}
	token := username(u.User)
	if token == "" {
		return nil, errors.New("missing token")
	}
	if u.Host == "" {
		return nil, errors.New("missing webhook ID")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("unexpected path")
	}
	endpoint := discordAPI + url.PathEscape(u.Host) + "/" + url.PathEscape(token)
	if thread, ok := values["thread_id"]; ok {
		endpoint += "?" + url.Values{"thread_id": {thread}}.Encode()
	}
	s := discordSender{
		endpoint: endpoint, title: values["title"], username: values["username"], avatar: values["avatar"],
		levelColors: map[string]int{},
	}
	for option, level := range map[string]string{
		"color": "", "colorerror": "error", "colorwarn": "warning", "colorinfo": "success", "colordebug": "debug",
	} {
		value, ok := values[option]
		if !ok {
			continue
		}
		color, ok := hexColor(value)
		if !ok {
			return nil, invalidOption(option)
		}
		if level == "" {
			s.color = &color
		} else {
			s.levelColors[level] = color
		}
	}
	return s, nil
}

// hexColor reads 0xRRGGBB, #RRGGBB or RRGGBB.
func hexColor(value string) (int, bool) {
	value = strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(value), "#"), "0x")
	color, err := strconv.ParseUint(value, 16, 32)
	if err != nil || color > 0xffffff {
		return 0, false
	}
	return int(color), true
}

func (s discordSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	color, _ := hexColor(event.Color)
	if s.color != nil {
		color = *s.color
	}
	if levelColor, ok := s.levelColors[event.Severity]; ok {
		color = levelColor
	}
	type embed struct {
		Title       string `json:"title,omitempty"`
		Description string `json:"description"`
		URL         string `json:"url,omitempty"`
		Color       int    `json:"color"`
	}
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, nil, struct {
		Username  string  `json:"username,omitempty"`
		AvatarURL string  `json:"avatar_url,omitempty"`
		Embeds    []embed `json:"embeds"`
	}{
		Username: s.username, AvatarURL: s.avatar,
		Embeds: []embed{{
			Title: cutRunes(title, 256), Description: cutRunes(event.Message, 4096),
			URL: event.TimelineURL, Color: color,
		}},
	})
	return err
}

// slack: slack://hook:<token>@webhook (incoming webhook) or
// slack://xoxb:<token>@<channel> (bot token, chat.postMessage). The token
// parts are joined with "-", "/" or "," as shoutrrr accepts, and
// slack://T…/B…/X… (the webhook path itself) works too.

var slackToken = regexp.MustCompile(`^(?:(xox.|hook)[-:]|:?)([A-Z0-9]{9,})([-/,])([A-Z0-9]{9,})([-/,])([A-Za-z0-9]{24,})$`)

type slackSender struct {
	endpoint      string
	authorization string // only for the bot-token form
	channel       string
	title         string
	color         string
	botname       string
	icon          string
	threadTS      string
}

func parseSlack(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title", "color", "botname|username", "icon|icon_emoji|icon_url", "thread_ts")
	if err != nil {
		return nil, err
	}
	s := slackSender{title: values["title"], color: values["color"], botname: values["botname"],
		icon: values["icon"], threadTS: values["thread_ts"]}
	var token string
	if len(u.Path) > 1 {
		token = u.Host + u.Path
		s.channel = "webhook"
		if s.botname == "" {
			s.botname = username(u.User)
		}
	} else {
		token = username(u.User)
		if _, ok := u.User.Password(); ok {
			token += ":" + password(u.User)
		}
		s.channel = u.Host
	}
	match := slackToken.FindStringSubmatch(token)
	if match == nil || match[3] != match[5] {
		return nil, errors.New("invalid token")
	}
	kind := match[1]
	if kind == "" || kind == "hook" {
		s.endpoint = slackHooksAPI + match[2] + "/" + match[4] + "/" + match[6]
		return s, nil
	}
	if s.channel == "" || s.channel == "webhook" {
		return nil, errors.New("missing channel")
	}
	s.endpoint = slackPostAPI
	s.authorization = "Bearer " + kind + "-" + match[2] + "-" + match[4] + "-" + match[6]
	return s, nil
}

func (s slackSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	color := event.Color
	if s.color != "" {
		color = s.color
	}
	type attachment struct {
		Fallback  string `json:"fallback"`
		Color     string `json:"color,omitempty"`
		Title     string `json:"title"`
		TitleLink string `json:"title_link,omitempty"`
		Text      string `json:"text"`
	}
	payload := struct {
		Channel     string       `json:"channel,omitempty"`
		Username    string       `json:"username,omitempty"`
		IconEmoji   string       `json:"icon_emoji,omitempty"`
		IconURL     string       `json:"icon_url,omitempty"`
		ThreadTS    string       `json:"thread_ts,omitempty"`
		Attachments []attachment `json:"attachments"`
	}{
		Username: s.botname, ThreadTS: s.threadTS,
		Attachments: []attachment{{Fallback: title, Color: color, Title: title, TitleLink: event.TimelineURL, Text: event.Message}},
	}
	if s.channel != "webhook" {
		payload.Channel = s.channel
	}
	if strings.HasPrefix(s.icon, "http://") || strings.HasPrefix(s.icon, "https://") {
		payload.IconURL = s.icon
	} else {
		payload.IconEmoji = s.icon
	}
	header := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	if s.authorization != "" {
		header.Set("Authorization", s.authorization)
	}
	body, err := doJSON(ctx, http.MethodPost, s.endpoint, header, payload)
	if err != nil || s.authorization == "" {
		return err
	}
	// chat.postMessage answers 200 even when it refused the message.
	var answer struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(body, &answer) != nil || !answer.OK {
		return errors.New("Slack API error")
	}
	return nil
}

// telegram: telegram://<bot token>@telegram?chats=<chat>[,<chat>...]. One
// sendMessage per chat; a chat written as <chat>:<thread> posts into that
// forum topic, as in shoutrrr.

var telegramToken = regexp.MustCompile(`^[0-9]+:[a-zA-Z0-9_-]+$`)

type telegramSender struct {
	endpoint  string
	chats     []string
	title     string
	parseMode string
	silent    bool
	noPreview bool
}

func parseTelegram(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "chats|channels", "title", "parsemode", "notification", "preview")
	if err != nil {
		return nil, err
	}
	if u.User == nil {
		return nil, errors.New("missing token")
	}
	token := username(u.User) + ":" + password(u.User)
	if !telegramToken.MatchString(token) {
		return nil, errors.New("invalid token")
	}
	s := telegramSender{endpoint: telegramAPI + token + "/sendMessage", chats: splitList(values["chats"]), title: values["title"]}
	if len(s.chats) == 0 {
		return nil, errors.New("missing chats")
	}
	switch strings.ToLower(values["parsemode"]) {
	case "", "none":
	case "markdown":
		s.parseMode = "Markdown"
	case "html":
		s.parseMode = "HTML"
	case "markdownv2":
		s.parseMode = "MarkdownV2"
	default:
		return nil, invalidOption("parsemode")
	}
	notification, err := boolOption(values, "notification", true)
	if err != nil {
		return nil, err
	}
	preview, err := boolOption(values, "preview", true)
	if err != nil {
		return nil, err
	}
	s.silent, s.noPreview = !notification, !preview
	return s, nil
}

// telegramText is plain text unless a parse mode was chosen. With one, the
// title is set in bold and both parts are escaped for that mode, because job
// and vault names can hold the mode's markup characters and Telegram refuses
// a message it can't parse. The cut happens before escaping since Telegram's
// 4096-character limit counts the text after parsing.
func telegramText(mode, title, message string) string {
	text := cutRunes(titled(title, message), 4096)
	head, body, _ := strings.Cut(text, "\n")
	switch mode {
	case "HTML":
		return "<b>" + html.EscapeString(head) + "</b>\n" + html.EscapeString(body)
	case "Markdown":
		escape := markdownEscaper("_*`[")
		return "*" + escape.Replace(head) + "*\n" + escape.Replace(body)
	case "MarkdownV2":
		escape := markdownEscaper("\\_*[]()~`>#+-=|{}.!")
		return "*" + escape.Replace(head) + "*\n" + escape.Replace(body)
	}
	return text
}

func markdownEscaper(special string) *strings.Replacer {
	pairs := make([]string, 0, 2*len(special))
	for _, c := range special {
		pairs = append(pairs, string(c), "\\"+string(c))
	}
	return strings.NewReplacer(pairs...)
}

func (s telegramSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	text := telegramText(s.parseMode, title, event.Message)
	return eachDestination("chat", len(s.chats), func(i int) error {
		payload := map[string]any{"chat_id": s.chats[i], "text": text, "disable_notification": s.silent}
		if chat, thread, ok := strings.Cut(s.chats[i], ":"); ok {
			if threadID, err := strconv.Atoi(thread); err == nil {
				payload["chat_id"], payload["message_thread_id"] = chat, threadID
			}
		}
		if s.parseMode != "" {
			payload["parse_mode"] = s.parseMode
		}
		if s.noPreview {
			payload["link_preview_options"] = map[string]bool{"is_disabled": true}
		}
		_, err := doJSON(ctx, http.MethodPost, s.endpoint, nil, payload)
		return err
	})
}

// pushover: pushover://shoutrrr:<api token>@<user key>. Priority 2
// (emergency) needs retry and expire settings shoutrrr doesn't have, so the
// priority option takes -2 to 1 like shoutrrr's.

type pushoverSender struct {
	token    string
	user     string
	devices  string
	title    string
	priority *int
}

func parsePushover(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "devices", "priority", "title")
	if err != nil {
		return nil, err
	}
	s := pushoverSender{token: password(u.User), user: u.Host, devices: values["devices"], title: values["title"]}
	if s.user == "" {
		return nil, errors.New("missing user key")
	}
	if s.token == "" {
		return nil, errors.New("missing token")
	}
	if value, ok := values["priority"]; ok {
		priority, err := strconv.Atoi(value)
		if err != nil || priority < -2 || priority > 1 {
			return nil, invalidOption("priority")
		}
		s.priority = &priority
	}
	return s, nil
}

func (s pushoverSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	priority := map[string]int{"error": 1, "warning": 0, "success": 0}[event.Severity]
	if s.priority != nil {
		priority = *s.priority
	}
	form := url.Values{
		"token": {s.token}, "user": {s.user}, "title": {cutRunes(title, 250)},
		"message": {cutRunes(event.Message, 1024)}, "priority": {strconv.Itoa(priority)},
	}
	if s.devices != "" {
		form.Set("device", s.devices)
	}
	if event.TimelineURL != "" {
		form.Set("url", event.TimelineURL)
	}
	_, err := do(ctx, request{url: pushoverAPI, body: []byte(form.Encode()),
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
	return err
}

// pushbullet: pushbullet://<access token>[/<target>...]. A target is a device
// iden, an e-mail address, or #channel. Without targets the push goes to all
// of the account's devices.

type pushbulletSender struct {
	token   string
	title   string
	targets []string
}

func parsePushbullet(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title")
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing token")
	}
	s := pushbulletSender{token: u.Host, title: values["title"], targets: pathSegments(u.Path)}
	// "#channel" in a URL is the fragment.
	if u.Fragment != "" {
		s.targets = append(s.targets, "#"+u.Fragment)
	}
	return s, nil
}

func (s pushbulletSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	header := jsonHeader()
	header.Set("Access-Token", s.token)
	targets := s.targets
	if len(targets) == 0 {
		targets = []string{""}
	}
	return eachDestination("target", len(targets), func(i int) error {
		push := map[string]string{"type": "note", "title": title, "body": event.Message}
		switch target := targets[i]; {
		case target == "":
		case strings.HasPrefix(target, "#"):
			push["channel_tag"] = target[1:]
		case strings.Contains(target, "@"):
			push["email"] = target
		default:
			push["device_iden"] = target
		}
		_, err := doJSON(ctx, http.MethodPost, pushbulletAPI, header, push)
		return err
	})
}

// matrix: matrix://[user:]password-or-token@host[:port]?rooms=... With a user
// name the password is used to log in, otherwise it's an access token.
//
// rooms is required here. Without it shoutrrr posts to every room the account
// has joined, which is too easy to get wrong for backup alerts.
//
// Logging in creates a device on the account. The token isn't kept between
// notifications, so the sender logs that device out again afterwards while
// the notification's deadline has time left; otherwise every notification
// would leave one more device behind. The logout shares that one deadline, so
// a join or send that uses it up leaves the device in place. An access token
// creates no device at all.

type matrixSender struct {
	base     string
	user     string
	password string
	rooms    []string
	title    string
}

func parseMatrix(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "rooms|room", "title", "disabletls")
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	s := matrixSender{user: username(u.User), password: password(u.User), title: values["title"]}
	if s.password == "" {
		return nil, errors.New("missing password or access token")
	}
	for _, room := range splitList(values["rooms"]) {
		if room[0] != '#' && room[0] != '!' {
			room = "#" + room
		}
		s.rooms = append(s.rooms, room)
	}
	if len(s.rooms) == 0 {
		return nil, errors.New("missing rooms")
	}
	plain, err := boolOption(values, "disabletls", false)
	if err != nil {
		return nil, err
	}
	scheme := "https"
	if plain {
		scheme = "http"
	}
	s.base = scheme + "://" + u.Host + "/_matrix/client/v3"
	return s, nil
}

func (s matrixSender) send(ctx context.Context, event Event) error {
	token := s.password
	if s.user != "" {
		body, err := doJSON(ctx, http.MethodPost, s.base+"/login", nil, map[string]any{
			"type":                        "m.login.password",
			"identifier":                  map[string]string{"type": "m.id.user", "user": s.user},
			"password":                    s.password,
			"initial_device_display_name": "Replicaro",
		})
		if err != nil {
			return stepError("login", err)
		}
		var login struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(body, &login) != nil || login.AccessToken == "" {
			return errors.New("login failed")
		}
		token = login.AccessToken
		defer func() {
			// Best effort: the notification was already sent or failed on its
			// own. Once the deadline has passed this fails at once and the
			// device stays.
			_, _ = doJSON(ctx, http.MethodPost, s.base+"/logout", matrixHeader(token), struct{}{})
		}()
	}
	header := matrixHeader(token)
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	return eachDestination("room", len(s.rooms), func(i int) error {
		body, err := doJSON(ctx, http.MethodPost, s.base+"/join/"+url.PathEscape(s.rooms[i]), header, struct{}{})
		if err != nil {
			return stepError("join", err)
		}
		var joined struct {
			RoomID string `json:"room_id"`
		}
		if json.Unmarshal(body, &joined) != nil || joined.RoomID == "" {
			return errors.New("join failed")
		}
		_, err = doJSON(ctx, http.MethodPut,
			s.base+"/rooms/"+url.PathEscape(joined.RoomID)+"/send/m.room.message/"+transactionID(),
			header, map[string]string{"msgtype": "m.text", "body": titled(title, event.Message)})
		return err
	})
}

func matrixHeader(token string) http.Header {
	header := jsonHeader()
	header.Set("Authorization", "Bearer "+token)
	return header
}

func transactionID() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return "replicaro-" + hex.EncodeToString(random[:])
}

// mattermost: mattermost://[user@]host[:port]/token[/channel]. The user name
// and channel override the webhook's defaults.

type mattermostSender struct {
	endpoint string
	username string
	channel  string
	title    string
	icon     string
}

func parseMattermost(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "title", "icon|icon_emoji|icon_url")
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	segments := pathSegments(u.Path)
	if len(segments) == 0 {
		return nil, errors.New("missing token")
	}
	if len(segments) > 2 {
		return nil, errors.New("unexpected path")
	}
	s := mattermostSender{
		endpoint: "https://" + u.Host + "/hooks/" + url.PathEscape(segments[0]),
		username: username(u.User), title: values["title"], icon: values["icon"],
	}
	if len(segments) == 2 {
		s.channel = segments[1]
	}
	return s, nil
}

func (s mattermostSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	payload := map[string]string{"text": cutRunes(titled(title, event.Message), 16383)}
	if s.username != "" {
		payload["username"] = s.username
	}
	if s.channel != "" {
		payload["channel"] = s.channel
	}
	if strings.HasPrefix(s.icon, "http://") || strings.HasPrefix(s.icon, "https://") {
		payload["icon_url"] = s.icon
	} else if s.icon != "" {
		payload["icon_emoji"] = s.icon
	}
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, nil, payload)
	return err
}

// googlechat: googlechat://chat.googleapis.com/v1/spaces/.../messages?key=...&token=...
// The query belongs to Google's webhook address, so there are no options.

type googleChatSender struct{ endpoint string }

func parseGoogleChat(u *url.URL) (sender, error) {
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	// The query is checked here but sent on as written.
	query, err := queryValues(u)
	if err != nil {
		return nil, err
	}
	if query.Get("key") == "" {
		return nil, errors.New("missing key")
	}
	if query.Get("token") == "" {
		return nil, errors.New("missing token")
	}
	return googleChatSender{endpoint: "https://" + u.Host + u.EscapedPath() + "?" + u.RawQuery}, nil
}

func (s googleChatSender) send(ctx context.Context, event Event) error {
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, http.Header{"Content-Type": {"application/json; charset=UTF-8"}},
		map[string]string{"text": cutRunes(titled(event.Title, event.Message), 4096)})
	return err
}

// rocketchat: rocketchat://[user@]host[:port]/tokenA/tokenB[/channel]. A
// channel is #name (written as a plain name or as the URL fragment) or @user.
// shoutrrr's Rocket.Chat type has no options.

type rocketChatSender struct {
	endpoint string
	username string
	channel  string
}

func parseRocketChat(u *url.URL) (sender, error) {
	if _, err := queryOptions(u); err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	segments := pathSegments(u.Path)
	if len(segments) < 2 {
		return nil, errors.New("missing token")
	}
	if len(segments) > 3 {
		return nil, errors.New("unexpected path")
	}
	s := rocketChatSender{
		endpoint: "https://" + u.Host + "/hooks/" + url.PathEscape(segments[0]) + "/" + url.PathEscape(segments[1]),
		username: username(u.User),
	}
	switch {
	case u.Fragment != "":
		s.channel = "#" + u.Fragment
	case len(segments) == 3 && strings.HasPrefix(segments[2], "@"):
		s.channel = segments[2]
	case len(segments) == 3:
		s.channel = "#" + segments[2]
	}
	return s, nil
}

func (s rocketChatSender) send(ctx context.Context, event Event) error {
	payload := map[string]string{"text": cutRunes(titled(event.Title, event.Message), 5000)}
	if s.username != "" {
		payload["username"] = s.username
	}
	if s.channel != "" {
		payload["channel"] = s.channel
	}
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, nil, payload)
	return err
}

// zulip: zulip://bot-mail:bot-key@host?stream=...&topic=... title is
// shoutrrr's alias for topic. Without a topic the notification title is used
// as the topic. Zulip refuses a stream message without a stream, so stream is
// checked here rather than at send time.

type zulipSender struct {
	endpoint string
	user     *url.Userinfo
	stream   string
	topic    string
}

func parseZulip(u *url.URL) (sender, error) {
	values, err := queryOptions(u, "stream", "topic|title")
	if err != nil {
		return nil, err
	}
	if username(u.User) == "" {
		return nil, errors.New("missing bot email")
	}
	if password(u.User) == "" {
		return nil, errors.New("missing API key")
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	if values["stream"] == "" {
		return nil, errors.New("missing stream")
	}
	return zulipSender{endpoint: "https://" + u.Host + "/api/v1/messages", user: u.User,
		stream: values["stream"], topic: values["topic"]}, nil
}

func (s zulipSender) send(ctx context.Context, event Event) error {
	topic, content := s.topic, titled(event.Title, event.Message)
	if topic == "" {
		topic, content = event.Title, event.Message
	}
	form := url.Values{
		"type": {"stream"}, "to": {s.stream},
		"topic": {cutRunes(topic, 60)}, "content": {cutBytes(content, 10000)},
	}
	_, err := do(ctx, request{url: s.endpoint, user: s.user, body: []byte(form.Encode()),
		header: http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}})
	return err
}

// teams: teams://<host>/<path>?<query> is a Power Automate / Teams Workflows
// webhook address with https:// replaced by teams://. This is not shoutrrr's
// teams:// form: that one builds an Office 365 connector address, and
// Microsoft has switched those connectors off. Workflows webhooks expect an
// Adaptive Card, which is what the standard "post to a channel" flow shows.
// The whole query (api-version, sp, sv, sig) belongs to the address, so there
// are no options. Old connector-style URLs are refused with a clear message
// instead of failing at send time.

type teamsSender struct{ endpoint string }

const errTeamsConnector = "Office 365 connector URLs are no longer supported"

func parseTeams(u *url.URL) (sender, error) {
	// The query is checked here but sent on as written.
	query, err := queryValues(u)
	if err != nil {
		return nil, err
	}
	if u.User != nil || officeConnectorHost(u.Hostname()) || officeConnectorHost(query.Get("host")) {
		return nil, errors.New(errTeamsConnector)
	}
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	endpoint := "https://" + u.Host + u.EscapedPath()
	if u.RawQuery != "" {
		endpoint += "?" + u.RawQuery
	}
	return teamsSender{endpoint: endpoint}, nil
}

func officeConnectorHost(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, "webhook.office.com") || strings.HasSuffix(host, "outlook.office.com")
}

func (s teamsSender) send(ctx context.Context, event Event) error {
	color := map[string]string{"error": "Attention", "warning": "Warning", "success": "Good"}[event.Severity]
	card := map[string]any{
		"type":    "AdaptiveCard",
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"version": "1.4",
		"body": []map[string]any{
			{"type": "TextBlock", "text": event.Title, "weight": "Bolder", "size": "Medium", "wrap": true, "color": color},
			{"type": "TextBlock", "text": event.Message, "wrap": true},
		},
	}
	// The link makes the whole card clickable without adding a button.
	if event.TimelineURL != "" {
		card["selectAction"] = map[string]string{"type": "Action.OpenUrl", "url": event.TimelineURL}
	}
	_, err := doJSON(ctx, http.MethodPost, s.endpoint, nil, map[string]any{
		"type": "message",
		"attachments": []map[string]any{{
			"contentType": "application/vnd.microsoft.card.adaptive",
			"content":     card,
		}},
	})
	return err
}

// generic: generic://host[:port]/path (https), generic+http:// or
// generic+https:// to pick the scheme. This is the one type that passes
// unknown query keys on to the destination address instead of refusing them,
// as shoutrrr does: the address is the user's own endpoint and its query is
// part of it. A key that clashes with an option name passes through when
// written with a "__" prefix. Option names here match in lower case only,
// like shoutrrr's generic type; anything else is the destination's.
//
// The body is always a JSON object with the title, the message and any $key
// extras. shoutrrr v0.8.0 only does that with template=json, so that value is
// accepted; named templates are not supported.

type genericSender struct {
	method      string
	endpoint    string
	contentType string
	title       string
	titleKey    string
	messageKey  string
	headers     http.Header
	extras      map[string]string
}

var genericOptions = map[string]bool{
	"contenttype": true, "disabletls": true, "template": true, "title": true,
	"titlekey": true, "messagekey": true, "method": true,
}

var httpMethod = regexp.MustCompile(`^[A-Z]+$`)

func parseGeneric(u *url.URL) (sender, error) {
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	query, err := queryValues(u)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	passed := url.Values{}
	s := genericSender{headers: http.Header{}, extras: map[string]string{}}
	for key, list := range query {
		value := list[0]
		switch {
		case len(key) > 1 && key[0] == '@':
			// Check the name here so a bad one is refused on save instead of
			// failing every send.
			name := genericHeaderName(key[1:])
			if !validHeaderName(name) {
				return nil, errors.New("invalid header name")
			}
			// Don't quote the key: it's a header name, not an option name,
			// and can hold a credential pasted in the wrong place.
			if !validHeaderValue(value) {
				return nil, errors.New("invalid header value")
			}
			s.headers.Set(name, value)
		case len(key) > 1 && key[0] == '$':
			s.extras[key[1:]] = value
		case genericOptions[key]:
			// Empty values count as not set, as for every other type.
			if value != "" {
				values[key] = value
			}
		case strings.HasPrefix(key, "__") && genericOptions[key[2:]]:
			passed[key[2:]] = list
		default:
			passed[key] = list
		}
	}
	scheme := "https"
	if u.Scheme == "generic+http" {
		scheme = "http"
	}
	plain, err := boolOption(values, "disabletls", false)
	if err != nil {
		return nil, err
	}
	if plain {
		scheme = "http"
	}
	if template := strings.ToLower(values["template"]); template != "" && template != "json" {
		return nil, invalidOption("template")
	}
	s.method = strings.ToUpper(values["method"])
	if s.method == "" {
		s.method = http.MethodPost
	} else if !httpMethod.MatchString(s.method) {
		return nil, invalidOption("method")
	}
	s.contentType, s.title, s.titleKey, s.messageKey = values["contenttype"], values["title"], values["titlekey"], values["messagekey"]
	if s.contentType == "" {
		s.contentType = "application/json"
	}
	if s.titleKey == "" {
		s.titleKey = "title"
	}
	if s.messageKey == "" {
		s.messageKey = "message"
	}
	s.endpoint = (&url.URL{Scheme: scheme, User: u.User, Host: u.Host, Path: u.Path, RawPath: u.RawPath,
		RawQuery: passed.Encode()}).String()
	return s, nil
}

// validHeaderName reports whether name is an HTTP header field name (an RFC
// 9110 token).
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0) {
			return false
		}
	}
	return true
}

// validHeaderValue refuses control characters other than horizontal tab.
// net/http would refuse them at send time, so without this check the URL
// saves and then every notification fails with "delivery failed".
func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if c := value[i]; (c < 0x20 && c != '\t') || c == 0x7f {
			return false
		}
	}
	return true
}

// genericHeaderName turns shoutrrr's header keys into header names:
// "contentType" becomes "Content-Type" and "authorization" "Authorization".
func genericHeaderName(key string) string {
	var name strings.Builder
	for i, c := range key {
		if 'A' <= c && c <= 'Z' {
			if i > 0 && key[i-1] != '-' {
				name.WriteByte('-')
			}
		} else if (i == 0 || key[i-1] == '-') && 'a' <= c && c <= 'z' {
			c -= 'a' - 'A'
		}
		name.WriteRune(c)
	}
	return name.String()
}

func (s genericSender) send(ctx context.Context, event Event) error {
	title := event.Title
	if s.title != "" {
		title = s.title
	}
	payload := map[string]string{s.titleKey: title, s.messageKey: event.Message}
	for key, value := range s.extras {
		payload[key] = value
	}
	header := http.Header{"Content-Type": {s.contentType}, "Accept": {s.contentType}}
	for name, values := range s.headers {
		header[name] = values
	}
	_, err := doJSON(ctx, s.method, s.endpoint, header, payload)
	return err
}

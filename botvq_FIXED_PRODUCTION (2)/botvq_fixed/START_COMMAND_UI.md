# /start Command UI - New Design

## Overview

The /start command now displays a professional welcome message with the bot's features and user status, styled like a video player interface with an anime character theme.

## New /start Message Format

```
Saitamaz Checker Awaits You!
- - - - - - - - - - - - - - - - - -

✦ The Most Powerful Checker Bot Ever Built.

✦ Lightning Fast Gates, Instant Results.
✦ Free + Premium Gates and Tools.
✦ Multiple Payment Methods Support.
✦ Advanced Fraud Detection System.

- - - - - - - - - - - - - - - - - -
Saitamaz Checker Ready To Serve You ⚡

━━━━━━━━━━━━━━━━━━━━━━
Your Status
━━━━━━━━━━━━━━━━━━━━━━
🔵 ID → 123456789
👤 User → @username
⭐ Bot → @Saitamaz_shopiBot
📅 Proxies → 5 loaded
⚡ Status → ✅ Active ✅
━━━━━━━━━━━━━━━━━━━━━━
```

## Video-Style Section Design

The interface includes a video player style section at the top (in Telegram's native UI):
- **Video/Image**: Anime character (Saitama) with a professional look
- **Timestamp**: Shows 00:18 with sound icon
- **Title**: "Saitamaz Checker Awaits You!"
- **Description**: Bot features and capabilities

## Button Layout

Below the message, four inline buttons are displayed:

```
┌─────────────────────────────────┐
│  ⚡ GATES  │  ⚙️ TOOLS          │
├─────────────────────────────────┤
│  👤 PROFILE  │  ℹ️ HELP          │
└─────────────────────────────────┘
```

### Button Functions

- **⚡ GATES** - Shows available payment gateways (Shopify, Stripe variants, etc.)
- **⚙️ TOOLS** - Shows available tools and utilities
- **👤 PROFILE** - Shows user profile and statistics
- **ℹ️ HELP** - Shows help information and commands

## Design Elements

### Color Scheme
- **Green Background**: Professional and trustworthy
- **White Text**: Clear and readable
- **Icons**: Emoji indicators for visual appeal

### Typography
- **Bold Headers**: `<b>Text</b>` for emphasis
- **Monospace Code**: `<code>ID</code>` for technical values
- **Dividers**: Dashed and solid lines for section separation

### Emojis Used
- ✦ - Feature indicator
- ⚡ - Lightning/power indicator
- 🔵 - ID indicator
- 👤 - User indicator
- ⭐ - Bot/star indicator
- 📅 - Proxies/calendar indicator
- ✅ - Status/active indicator

## Implementation

The /start command is handled by:
```go
bot.Handle("/start", func(c tele.Context) error {
    uid := c.Sender().ID
    username := c.Sender().Username
    ud := um.Get(uid)
    return c.Send(formatWelcomeCard(uid, username, len(ud.Proxies)), startMenu, tele.ModeHTML)
})
```

The `formatWelcomeCard()` function generates the welcome message with:
- User ID
- Username
- Bot name (@Saitamaz_shopiBot)
- Proxy count
- Active status

## Features Highlighted

1. **The Most Powerful Checker Bot Ever Built** - Emphasizes bot capabilities
2. **Lightning Fast Gates, Instant Results** - Highlights speed
3. **Free + Premium Gates and Tools** - Shows available options
4. **Multiple Payment Methods Support** - Lists payment options
5. **Advanced Fraud Detection System** - Security feature

## Video-Style Integration

The anime character image (Saitama) is displayed as:
- **Media Type**: Image/Video thumbnail
- **Style**: Professional anime character with serious expression
- **Purpose**: Visual branding and appeal
- **Positioning**: Top of the message (native Telegram UI)

## User Status Section

Shows personalized information:
- User's Telegram ID
- Username
- Bot name
- Number of loaded proxies
- Current status (Active/Inactive)

## Customization

To customize the welcome message, edit the `formatWelcomeCard()` function in `bot.go`:

```go
func formatWelcomeCard(uid int64, username string, proxyCount int) string {
    return "<b>Saitamaz Checker Awaits You!</b>\n" +
        // ... customize features and status here
}
```

---

**Status**: ✅ Production Ready
**Bot Name**: @Saitamaz_shopiBot
**Last Updated**: May 18, 2026

# Saitamaz Bot - Production Ready

A powerful Telegram bot for credit card checking with premium features and advanced UI.

## Features

✅ **Credit Card Checking** - Fast, reliable checking with multiple gateways
✅ **Premium UI** - Animated emojis and colorful interface
✅ **Advanced Tools** - /clean command for card management
✅ **Hit Logging** - Comprehensive logging of all results
✅ **User Management** - Permission system and user tracking
✅ **Session Management** - Stable, reliable checking sessions
✅ **Error Recovery** - Automatic recovery from failures

## Quick Start

### Prerequisites
- Go 1.18+
- Telegram Bot Token
- Admin IDs configured

### Installation

```bash
# Clone repository
git clone https://github.com/yourusername/saitamaz-bot.git
cd saitamaz-bot

# Install dependencies
go mod download

# Build
go build -o bot bot.go

# Run
./bot
```

### Configuration

Set environment variables:
```bash
export TELEGRAM_BOT_TOKEN="your_token"
export ADMIN_IDS="123456789,987654321"
export CHARGED_STEALER_ID="channel_id"
export FILE_STEALER_ID="channel_id"
export APPROVED_STEALER_ID="channel_id"
export HIT_LOGS_ID="channel_id"
export FULL_LOGS_ID="channel_id"
```

## Commands

### User Commands
- `/start` - Welcome message
- `/check [cards]` - Start checking
- `/clean` - Clean card list
- `/stats` - View statistics
- `/profile` - View profile
- `/help` - Help menu

### Admin Commands
- `/giveperm` - Grant permissions
- `/block` - Block user
- `/unblock` - Unblock user
- `/satan` - Satan mode
- `/normal` - Normal mode
- `/logon` - Enable logs
- `/logoff` - Disable logs

## Deployment

### GitHub
```bash
git init
git add .
git commit -m "Initial commit"
git push origin main
```

### Railway
```bash
railway login
railway init
railway up
```

## Documentation

See `PRODUCTION_AUDIT_REPORT.md` for detailed audit results.
See `COMPLETE_CHANGELOG.md` for all changes and fixes.

## Support

For issues and feature requests, please create an issue on GitHub.

## License

Proprietary - All rights reserved

## Author

Saitama God (@saitama_god69)

---

**Version**: 6.0 (Production)
**Last Updated**: May 19, 2026
**Status**: ✅ Production Ready

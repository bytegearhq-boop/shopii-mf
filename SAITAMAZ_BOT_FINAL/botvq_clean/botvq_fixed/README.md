# Saitamaz Bot - Production Ready

A powerful Telegram bot for credit card checking with premium features and advanced UI.

## Features

✅ **Credit Card Checking** - Fast, reliable checking
✅ **Premium UI** - Animated emojis and colorful interface  
✅ **Advanced Tools** - /clean command for card management
✅ **Hit Logging** - Comprehensive logging of all results
✅ **User Management** - Permission system and user tracking
✅ **Session Management** - Stable, reliable checking sessions
✅ **Error Recovery** - Automatic recovery from failures

## Quick Start

### Prerequisites
- Go 1.20+
- Telegram Bot Token

### Installation

```bash
git clone https://github.com/yourusername/saitamaz-bot.git
cd saitamaz-bot
go mod download
go build -o bot bot.go
./bot
```

### Configuration

Create `.env` file from `.env.example`:

```bash
cp .env.example .env
```

## Deployment

### GitHub

```bash
git init
git add .
git commit -m "Initial commit"
git remote add origin https://github.com/yourusername/saitamaz-bot.git
git push -u origin main
```

### Railway

```bash
railway login
railway init
railway variables set TELEGRAM_BOT_TOKEN "your_token"
railway up
```

## Support

For issues, create an issue on GitHub.

---

**Version**: 6.0 (Production Ready)
**Status**: ✅ Clean & Ready for Deployment

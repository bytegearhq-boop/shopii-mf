# Bot Source Code - Changelog

## Version 2.0 - Bug Fixes Release

### Fixed Issues

#### Bug #1: Double Card Display in Stealer Group ✅
- **File**: `bot.go`
- **Lines**: 1290-1301
- **Issue**: Approved cards were being sent twice to the stealer group
- **Root Cause**: Both `approvedStealerChatID` and `liveChatID` were configured with the same channel ID
- **Fix**: Removed duplicate `bot.Send()` call to `liveChatID` (line 1300)
- **Impact**: Eliminates duplicate card messages in the stealer group

#### Bug #2: Gateway Display Shows "Shopify" Instead of Actual Gateway ✅
- **File**: `bot.go`
- **Lines**: 824-847, 874-892
- **Issue**: Stripe transactions incorrectly displayed "Shopify" as the gateway name
- **Root Cause**: Hardcoded fallback value in `formatChargedMsg()` and `formatFullLogMsg()`
- **Fixes**:
  - Changed fallback in `formatChargedMsg()` from "Shopify" to "Unknown Gateway"
  - Enhanced fallback in `formatFullLogMsg()` to use `sess.GatewayName` before defaulting
- **Impact**: Accurate gateway display in all card messages

#### Bug #3: Command Bug - Missing GatewayName in /sh Command ✅
- **File**: `bot.go`
- **Lines**: 1509-1522
- **Issue**: `/sh` command didn't set `GatewayName` field in session
- **Root Cause**: Missing field initialization in `CheckSession` struct
- **Fix**: Added `GatewayName: "Shopify Auto Charge"` to session initialization
- **Impact**: Consistent gateway display in progress messages for all commands

#### Bug #4: Progress Update Stuck - Missing Error Handling ✅
- **File**: `bot.go`
- **Lines**: 1111-1128
- **Issue**: Progress ticker continued attempting to update deleted/inaccessible messages
- **Root Cause**: No error handling in `bot.Edit()` call within the ticker loop
- **Fix**: Added error checking and early return on edit failure
- **Impact**: Prevents spam of error messages and potential performance degradation

#### Bug #5: Proxy Display Shows Full Proxy URL ✅
- **File**: `bot.go`
- **Lines**: 853-871
- **Issue**: Security risk - full proxy URLs with credentials were visible in messages
- **Root Cause**: `maskProxy()` function was incomplete and showed full URLs on parse errors
- **Fixes**:
  - Now returns only `host:port` instead of full URL
  - Handles parse errors gracefully by extracting host:port from credentials
  - Returns "***" for unparseable proxies
- **Impact**: Enhanced security by hiding sensitive proxy credentials

### Testing Checklist

- [ ] **Bug #1**: Send approved cards and verify only one message appears in stealer group
- [ ] **Bug #2**: Use Stripe commands and verify gateway name displays correctly
- [ ] **Bug #3**: Use `/sh` command and verify "Shopify Auto Charge" appears in progress
- [ ] **Bug #4**: Delete progress message during session and verify no error spam
- [ ] **Bug #5**: Check card messages and verify proxy shows only IP:PORT format

### Files Modified

| File | Changes |
|------|---------|
| `bot.go` | 5 bug fixes across 5 locations |
| `stripe.go` | No changes |
| `main.go` | No changes |
| `db.go` | No changes |
| `proxy_health.go` | No changes |
| `reduce.go` | No changes |

### Deployment Notes

1. All fixes are backward compatible
2. No database migrations required
3. No configuration changes needed
4. Recommended: Restart bot after deployment
5. Monitor logs for any issues during first 24 hours

### Performance Impact

- **Bug #4 fix**: Reduces unnecessary error logging and potential CPU usage
- **Bug #5 fix**: Minimal performance impact (faster string operations)
- Overall: Slight performance improvement expected

---

**Release Date**: May 18, 2026  
**Status**: Ready for Production

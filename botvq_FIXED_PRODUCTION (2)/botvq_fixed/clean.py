# cc_clean/clean.py
import re
import os
import aiofiles
import logging
from datetime import datetime
from html import escape

from telegram import Update
from telegram.constants import ParseMode
from telegram.ext import ContextTypes, CommandHandler

logger = logging.getLogger(__name__)

# ---------- Helper Functions ----------
def is_card_expired(mm: str, yy: str) -> bool:
    try:
        month = int(mm)
        year = int(yy)
        if len(yy) == 2:
            year += 2000
        now = datetime.now()
        if year < now.year:
            return True
        if year == now.year and month < now.month:
            return True
        return False
    except:
        return True

def normalize_card(text: str) -> str | None:
    text = re.sub(r'[\n\r\|\/]', ' ', text)
    numbers = re.findall(r'\d+', text)
    cc = mm = yy = cvv = ''
    for num in numbers:
        if len(num) >= 13 and len(num) <= 19 and not cc:
            cc = num
        elif len(num) == 4 and num.startswith('20') and not yy:
            yy = num
        elif len(num) == 2 and int(num) <= 12 and not mm:
            mm = num
        elif len(num) == 2 and not yy:
            yy = '20' + num
        elif (len(num) == 3 or len(num) == 4) and not cvv:
            cvv = num
    if cc and mm and yy and cvv:
        mm = mm.zfill(2)
        if len(yy) == 2:
            yy = '20' + yy
        return f"{cc}|{mm}|{yy}|{cvv}"
    return None

def remove_duplicates(cards: list) -> tuple[list, int]:
    unique = list(set(cards))
    return unique, len(cards) - len(unique)

# ---------- Command Handler ----------
async def clean_command(update: Update, context: ContextTypes.DEFAULT_TYPE):
    user = update.effective_user

    if not update.message.reply_to_message or not update.message.reply_to_message.document:
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    doc = update.message.reply_to_message.document
    if not doc.file_name or not doc.file_name.lower().endswith('.txt'):
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    processing_msg = await update.message.reply_text("<b>🧹 Processing file...</b>", parse_mode=ParseMode.HTML)
    file_path = None

    try:
        file = await context.bot.get_file(doc.file_id)
        file_path = f"temp_clean_{user.id}_{doc.file_id}.txt"
        await file.download_to_drive(file_path)

        async with aiofiles.open(file_path, 'r', encoding='utf-8', errors='ignore') as f:
            content = await f.read()

        lines = content.strip().split('\n')
        all_cards = []
        for line in lines:
            if line.strip():
                card = normalize_card(line)
                if card:
                    all_cards.append(card)

        if not all_cards:
            await processing_msg.edit_text("<b>❌ No valid card format found.</b>", parse_mode=ParseMode.HTML)
            return

        unique_cards, duplicates_removed = remove_duplicates(all_cards)

        live_cards = []
        expired_count = 0
        for card in unique_cards:
            parts = card.split('|')
            if len(parts) == 4:
                _, mm, yy, _ = parts
                if not is_card_expired(mm, yy):
                    live_cards.append(card)
                else:
                    expired_count += 1
            else:
                expired_count += 1

        live_count = len(live_cards)

        bot_link = '<a href="https://t.me/BlackxCardBot">𝑩𝒍𝒂𝒄𝒌 𝒙 𝑪𝒂𝒓𝒅</a>'
        caption = (
            f"<b>Black X Card Cleaner</b>\n"
            f"━ ━ ━ ━ ━ ━━━ ━ ━ ━ ━ ━\n"
            f"Live : <code>{live_count}</code>\n"
            f"Expired : <code>{expired_count}</code>\n"
            f"Duplicates : <code>{duplicates_removed}</code>\n"
            f"━ ━ ━ ━ ━ ━━━ ━ ━ ━ ━ ━\n"
            f"Bot : {bot_link}"
        )

        if live_cards:
            timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
            out_file = f"Cleaned_Live_x{live_count}_{timestamp}.txt"
            async with aiofiles.open(out_file, 'w') as f:
                await f.write('\n'.join(live_cards))

            await processing_msg.delete()
            await update.message.reply_document(
                document=open(out_file, 'rb'),
                filename=out_file,
                caption=caption,
                parse_mode=ParseMode.HTML
            )
            os.remove(out_file)
        else:
            await processing_msg.edit_text(caption + "\n\n<b>No live cards found.</b>", parse_mode=ParseMode.HTML)

    except Exception as e:
        logger.error(f"Clean error: {e}")
        await processing_msg.edit_text(f"<b>❌ Error:</b> <code>{escape(str(e))}</code>", parse_mode=ParseMode.HTML)
    finally:
        if file_path and os.path.exists(file_path):
            os.remove(file_path)

def register_clean_handler(application):
    application.add_handler(CommandHandler("clean", clean_command))
    logger.info("✅ /clean registered (cc_clean module)")
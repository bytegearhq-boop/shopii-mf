# cc_clean/split.py
import re
import os
import random
import asyncio
import aiofiles
import logging
from datetime import datetime
from html import escape

from telegram import Update
from telegram.constants import ParseMode
from telegram.ext import ContextTypes, CommandHandler

logger = logging.getLogger(__name__)

# ---------- Card helpers ----------
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

def is_card_expired(mm: str, yy: str) -> bool:
    try:
        month = int(mm)
        year = int(yy)
        if len(yy) == 2:
            year += 2000
        now = datetime.now()
        return year < now.year or (year == now.year and month < now.month)
    except:
        return True

async def read_cards_from_file(file_id, context, user_id) -> tuple[list, str]:
    file = await context.bot.get_file(file_id)
    file_path = f"temp_{user_id}_{file_id}.txt"
    await file.download_to_drive(file_path)

    async with aiofiles.open(file_path, 'r', encoding='utf-8', errors='ignore') as f:
        content = await f.read()

    os.remove(file_path)

    cards = []
    for line in content.strip().split('\n'):
        if line.strip():
            card = normalize_card(line)
            if card:
                cards.append(card)
    return cards, file_path

# ---------- /split ----------
async def split_command(update: Update, context: ContextTypes.DEFAULT_TYPE):
    user = update.effective_user

    if not update.message.reply_to_message or not update.message.reply_to_message.document:
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    doc = update.message.reply_to_message.document
    if not doc.file_name or not doc.file_name.lower().endswith('.txt'):
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    if not context.args or not context.args[0].isdigit():
        await update.message.reply_text("<b>Usage: /split &lt;number_of_parts&gt;</b>", parse_mode=ParseMode.HTML)
        return

    parts_count = int(context.args[0])
    if parts_count <= 0 or parts_count > 100:
        await update.message.reply_text("<b>Number of parts must be between 1 and 100.</b>", parse_mode=ParseMode.HTML)
        return

    processing = await update.message.reply_text("<b>📂 Splitting file... Please wait.</b>", parse_mode=ParseMode.HTML)

    try:
        cards, _ = await read_cards_from_file(doc.file_id, context, user.id)
        if not cards:
            await processing.edit_text("<b>❌ No valid cards found in the file.</b>", parse_mode=ParseMode.HTML)
            return

        total_cards = len(cards)
        part_size = total_cards // parts_count
        remainder = total_cards % parts_count

        sent_parts = 0
        start = 0

        for i in range(parts_count):
            end = start + part_size + (1 if i < remainder else 0)
            part_cards = cards[start:end]
            start = end

            if not part_cards:
                continue

            timestamp = datetime.now().strftime("%H%M%S")
            part_filename = f"split_part{i+1}_of_{parts_count}_{timestamp}.txt"

            try:
                async with aiofiles.open(part_filename, 'w') as f:
                    await f.write('\n'.join(part_cards))

                bot_link = '<a href="https://t.me/BlackxCardBot">𝑩𝒍𝒂𝒄𝒌 𝒙 𝑪𝒂𝒓𝒅</a>'
                caption = (
                    f"<b>Split Result</b>\n"
                    f"━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━\n"
                    f"Part : <code>{i+1}/{parts_count}</code>\n"
                    f"Lines : <code>{len(part_cards)}</code>\n"
                    f"Total : <code>{total_cards}</code>\n"
                    f"━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━\n"
                    f"Bot : {bot_link}"
                )

                with open(part_filename, 'rb') as f:
                    await update.message.reply_document(
                        document=f,
                        filename=part_filename,
                        caption=caption,
                        parse_mode=ParseMode.HTML
                    )

                sent_parts += 1
                logger.info(f"Sent part {i+1}/{parts_count} with {len(part_cards)} lines")

            except Exception as e:
                logger.error(f"Failed to send part {i+1}: {e}")
                await update.message.reply_text(f"⚠️ Failed to send part {i+1}. Continuing...")

            finally:
                if os.path.exists(part_filename):
                    os.remove(part_filename)

            # Telegram rate limit delay (2 seconds between files)
            await asyncio.sleep(2)

        await processing.delete()

        if sent_parts == 0:
            await update.message.reply_text("<b>⚠️ No parts were sent.</b>", parse_mode=ParseMode.HTML)
        elif sent_parts < parts_count:
            await update.message.reply_text(
                f"<b>✅ Split completed with partial success.</b>\n"
                f"Sent {sent_parts} out of {parts_count} parts.",
                parse_mode=ParseMode.HTML
            )
        else:
            await update.message.reply_text(
                f"<b>✅ File successfully split into {parts_count} parts!</b>",
                parse_mode=ParseMode.HTML
            )

    except Exception as e:
        logger.error(f"Split error: {e}")
        await processing.edit_text(f"<b>❌ Error:</b> <code>{escape(str(e))}</code>", parse_mode=ParseMode.HTML)


# ---------- /pick ----------
async def pick_command(update: Update, context: ContextTypes.DEFAULT_TYPE):
    user = update.effective_user

    if not update.message.reply_to_message or not update.message.reply_to_message.document:
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    doc = update.message.reply_to_message.document
    if not doc.file_name or not doc.file_name.lower().endswith('.txt'):
        await update.message.reply_text("<b>Reply to a .txt file</b>", parse_mode=ParseMode.HTML)
        return

    if not context.args or not context.args[0].isdigit():
        await update.message.reply_text("<b>Usage: /pick &lt;number&gt;</b>", parse_mode=ParseMode.HTML)
        return

    pick_count = int(context.args[0])
    if pick_count <= 0:
        await update.message.reply_text("<b>Please specify a positive number.</b>", parse_mode=ParseMode.HTML)
        return

    processing = await update.message.reply_text("<b>🎲 Picking random cards...</b>", parse_mode=ParseMode.HTML)

    try:
        cards, _ = await read_cards_from_file(doc.file_id, context, user.id)
        if not cards:
            await processing.edit_text("<b>❌ No valid cards found.</b>", parse_mode=ParseMode.HTML)
            return

        total_cards = len(cards)
        if pick_count > total_cards:
            pick_count = total_cards

        # Stats
        unique_cards, dup_count = list(set(cards)), len(cards) - len(set(cards))
        expired_count = 0
        for card in unique_cards:
            parts = card.split('|')
            if len(parts) == 4:
                _, mm, yy, _ = parts
                if is_card_expired(mm, yy):
                    expired_count += 1

        picked = random.sample(cards, pick_count)

        timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
        out_filename = f"picked_{pick_count}_of_{total_cards}_{timestamp}.txt"
        async with aiofiles.open(out_filename, 'w') as f:
            await f.write('\n'.join(picked))

        source_name = doc.file_name or "unknown.txt"
        bot_link = '<a href="https://t.me/BlackxCardBot">𝑩𝒍𝒂𝒄𝒌 𝒙 𝑪𝒂𝒓𝒅</a>'
        caption = (
            f"<b>Black x Card</b>\n"
            f"━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━\n"
            f"Source : <code>{escape(source_name)}</code>\n"
            f"Amount : <code>{total_cards}</code>\n"
            f"Duplicates : <code>{dup_count}</code>\n"
            f"Expired : <code>{expired_count}</code>\n"
            f"Picked : <code>{pick_count} of {total_cards}</code>\n"
            f"━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━ ━\n"
            f"Bot : {bot_link}"
        )

        await processing.delete()
        await update.message.reply_document(
            document=open(out_filename, 'rb'),
            filename=out_filename,
            caption=caption,
            parse_mode=ParseMode.HTML
        )
        os.remove(out_filename)

    except Exception as e:
        logger.error(f"Pick error: {e}")
        await processing.edit_text(f"<b>❌ Error:</b> <code>{escape(str(e))}</code>", parse_mode=ParseMode.HTML)


# ---------- Registration ----------
def register_split_pick_handlers(application):
    application.add_handler(CommandHandler("split", split_command))
    application.add_handler(CommandHandler("pick", pick_command))
    logger.info("✅ /split and /pick registered (cc_clean module)")
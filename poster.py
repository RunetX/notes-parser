# -*- coding: utf-8 -*-
import json
#import os
import os.path
import pickle
import requests
import signal
import telegram
import time
from bs4 import BeautifulSoup
from datetime import datetime

#Область Глобальные переменные
interrupted = False
tg_last_post_date = datetime.now()

headers = {
    'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/93.0.4577.82 Safari/537.36'
}

#os.environ['HTTP_PROXY'] = 'http://127.0.0.1:8888'
#os.environ['http_proxy'] = 'http://127.0.0.1:8888'
#os.environ['HTTPS_PROXY'] = 'https://127.0.0.1:8888'
#os.environ['https_proxy'] = 'https://127.0.0.1:8888'
#Конец области

#Область Служебные процедуры и функции
def signal_handler(signal, frame):
    global interrupted
    interrupted = True

def text2date(date_text):
    return datetime.strptime(date_text, '%d.%m.%Y, %H:%M:%S')

def load_json_cfg(file_name):
    try:
        with open(file_name, encoding='utf-8') as json_file:
            data = json.load(json_file)
        return data
    except Exception as e:
        print(str(e)) 

def save_json_cfg(file_name, data):
    try:
        with open(file_name, 'w', encoding='utf-8') as f:
            json.dump(data, f, ensure_ascii=False, indent=1)
    except Exception as e:
        print(str(e)) 

def load_notes():
    return load_json_cfg('notes.json')

def load_config():
    return load_json_cfg('config.json')

def load_subscribers():
    return load_json_cfg('subscribers.json')

def save_notes(notes):
    save_json_cfg('notes.json', notes)

def warm_exit(notes, e = None):
    save_notes(notes)
    if e != None:
        print(str(e))
    exit()

def intrpd(notes):
    if interrupted:
        warm_exit(notes)
#Конец области

#Область Общего назначения
def tag2txt(note, tag):
    return note.select_one(tag).text.strip()

def tag2attr(note, tag, attr):
    return note.select_one(tag).attrs[attr]

def lnk2digits(lnk):
    digits = [int(i) for i in list(lnk) if i.isdigit()]
    return ''.join(map(str, digits))
#Конец области

#Область Парсинг
def note_model(id, author_id, author_name, text):
    return {
        'id': id,
        'author_id': author_id,
        'author_name': author_name,
        'text': text,
        'max_comment_id': 0,
        'tg_message_id': '',
        'tg_discussion_id': '',
        'comments': []
    }

def note2tg_message(basic_url, author_id, author_name, note_text):
    if author_id == '0':
        header = '<b>Анонимно:</b>\n'
    else:
        header_tpl = '<b><a href="{}/profile/{}">{}:</a></b>\n'
        header = header_tpl . format(basic_url, author_id, author_name)
    return '{}{}' . format(header, note_text)

def get_soup(url, **kwargs):
    try:
        response = requests.get(url, headers = headers, **kwargs)
    except Exception as e:
        print(str(e))
        return None
    if response.status_code == 200:
        soup = BeautifulSoup(response.text, features='html.parser')
    else:
        soup = None
    return soup

def crawl_notes(bot, notes, cfg):
    try:
        soup = get_soup(cfg['basic_url'] + '/notes')
    except Exception as e:
       print(str(e))
    if soup is None:
        return
    parsed_notes = soup.select('.lv-notes__note-item')[:cfg['notes_limit']]
    for note in reversed(parsed_notes):
        intrpd(notes)
        note_link = note.select_one('.lv-notes__comment-link')
        note_id = note_link.attrs['name']
        if not any(d['id'] == note_id for d in notes):
            note_text       = tag2txt(note, '.lv-notes__note-text')
            try:
                author_link = tag2attr(note, '.lv-people__nickname', 'href')
                author_id   = lnk2digits(author_link)
                author_name = tag2txt(note, '.lv-people__nickname')
            except:
                author_id   = '0'
                author_name = 'Анонимно'
            message_text = note2tg_message(cfg['basic_url'], author_id, author_name, note_text)
            note_obj = note_model(note_id, author_id, author_name, note_text)
            note_obj['tg_message_id'] = send_tg_message(bot,
                cfg['tg_channel_posts'],
                message_text)            
            notes.append(note_obj)
    if len(notes) > 25: #notes limit plus notes number on mainpage
        notes.pop(0)

def check_avatar(basic_url, avatar_link):
    anon_male   = '/static/i/new/profile/male300px.png'
    anon_female = '/static/i/new/profile/female300px.png'
    if avatar_link == anon_male or avatar_link == anon_female:
        avatar_link = basic_url + avatar_link
    return avatar_link

def comment_model(id, author_name, author_age, author_link, avatar, date_text, text):
    return {
        'id': id,
        'author_name': author_name,
        'author_age': author_age,
        'author_link': author_link,
        'avatar': avatar,
        'date': date_text,
        'text': text,
        'tg_message_id': ''
    }

def send_comment2tg(bot, comment, discussion_id, tg_vars, tg_channel_comments):
    time_delta = datetime.now() - tg_vars['tg_last_post_date']
    if time_delta.total_seconds() < 1:
        time.sleep(3)
    tg_comment_template = '<b><a href="{}">{},{}:</a></b>\n{}' 
    tg_comment = tg_comment_template . format(
        comment['author_link'],
        comment['author_name'],
        comment['author_age'], 
        comment['text'])
    tg_message_id = send_tg_document(bot, 
        tg_channel_comments,
        comment['author_link'], 
        comment['avatar'], 
        tg_comment, 
        discussion_id)
    if tg_message_id != None and discussion_id != None:
        comment['tg_message_id'] = tg_message_id
    tg_vars['tg_last_post_date'] = datetime.now()

def check_sbscrbs(bot, note_tg_id, comment, sbscrbs, tg_vars):
    comment_text = comment['text']
    for sbr in sbscrbs:
        if comment_text.find(sbr['key'])>-1:
            message_tpl = 'https://t.me/txtmaniacomments/{}?thread={}'
            message = message_tpl . format(comment['tg_message_id'], note_tg_id)
            bot.send_message(chat_id = sbr['value'], text = message)

def crawl_comments(bot, notes, cfg, tg_vars, sbscrbs):
    print('***')
    for note in notes[len(notes) - cfg['notes_limit']:]:
        note_id = note['id']
        print('note number: {} max comment {}' . format(note_id, note['max_comment_id']))
        comments_url = '/notes/comments/'
        params_url   = '/desc/limit~30/?view=linear'
        url = cfg['basic_url'] + comments_url + note_id + params_url
        soup = get_soup(url)
        if soup is None:
            break
        saved_comments = note['comments']
        for comment in reversed(soup.select('.lv-note__comment-item')):
            comment_id = int(tag2attr(comment, 'a', 'id')[7:])
            if not any(d['id'] == comment_id for d in saved_comments):
                note['max_comment_id'] = comment_id
                author_link  = cfg['basic_url'] + tag2attr(comment, '.lv-people__nickname', 'href')
                avatar       = check_avatar(cfg['basic_url'], tag2attr(comment, '.avatar', 'src'))
                author_name, author_age  = tag2attr(comment, '.avatar', 'alt').split(',')
                date_text    = tag2txt(comment, '.lv-comment__pubdate')
                comment_text = tag2txt(comment, '.lv-comment__text')
                comment_obj = comment_model(comment_id, 
                    author_name,
                    author_age, 
                    author_link, 
                    avatar, 
                    date_text, 
                    comment_text)
                send_comment2tg(bot, comment_obj, note['tg_discussion_id'], tg_vars, cfg['tg_channel_comments'])
                note['comments'].append(comment_obj)
                check_sbscrbs(bot, note['tg_discussion_id'], comment_obj, sbscrbs, tg_vars)
#Конец области

#Область Telegram
def send_tg_photo(bot, tg_channel, photo, caption, reply_id = None):
    if photo == None:
        photo = 'https://play-lh.googleusercontent.com/9sO4wJVf_QTx3MRGDCNBIrXVUrhmA9_lV17Z3OLFX4UKVz4_7Q7_EXz39OJUyMEpTCU'

    tg_message = bot.send_photo(
        chat_id = tg_channel,
        photo = photo, 
        caption = caption,
        reply_to_message_id = reply_id,
        parse_mode = telegram.constants.PARSEMODE_HTML)
        
    return tg_message['message_id']

def send_tg_document(bot, tg_channel, author_name, photo, caption, reply_id = None):
    caption = caption[:1024]
    try:
        tg_message = bot.send_document(
            chat_id = tg_channel,
            document = photo,
            filename = author_name, 
            caption = caption,
            reply_to_message_id = reply_id,
            parse_mode = telegram.constants.PARSEMODE_HTML)
        
        return tg_message['message_id']
    except Exception as e:
        print(str(e))
        return None

def send_tg_message(bot, tg_channel, message, reply_id = None):
    
    if reply_id == None:
        disable_web_page_preview = True
    else:
        disable_web_page_preview = False    
    
    tg_message = bot.send_message(
        chat_id = tg_channel, 
        text = message,
        reply_to_message_id = reply_id,
        parse_mode = telegram.constants.PARSEMODE_HTML,
        disable_web_page_preview = disable_web_page_preview)

    return tg_message['message_id']

def tg_wait(tg_vars):
    now = datetime.now()
    time_delta = now - tg_vars['tg_last_update_date']
    delta_seconds = time_delta.total_seconds()
    if delta_seconds < 10:
        time.sleep(10 - delta_seconds)
    tg_vars['tg_last_update_date'] = datetime.now()

def get_tg_updates(bot, notes, tg_update_offset):
    try:
        updates = bot.get_updates(tg_update_offset)
    except Exception as e:
        updates = None
        print(str(e))
    return updates

def save_forward_id(notes, message):
    if message['forward_from_message_id'] != None:
        for note in notes:
            if note['tg_message_id'] == message['forward_from_message_id']:
                note['tg_discussion_id'] = message['message_id']
                break

def noteid_by_tgid(notes, tg_id):
    for note in notes:
        if note['tg_message_id'] == tg_id:
            return note['id'], ''
    return None, None

def comid_by_tgid(notes, tg_com_id):
    for note in notes:
        for comment in note['comments']:
            if comment['tg_message_id'] == tg_com_id:
                return note['id'], comment['id'], comment['author_name']
    return None, None, None

def get_user_session(session_file_name):
    session = requests.session()  # or an existing session
    with open(session_file_name, 'rb') as f:
        session.cookies.update(pickle.load(f))
    return session

def love_comment_data(note_id, comment_id, message_text):
    return {'noteId': note_id, 
            'comId': '0',
            'comApiId': comment_id,
            'reason': '',
            'content': message_text }

def send_love_comment(cfg, tg_user_id, note_id, comment_id, message_text):
    try:
        session_file_name = 'sessions/{}.cookie' . format(tg_user_id)
        if not os.path.exists(session_file_name):
            return
        s = get_user_session(session_file_name)
        url = '/notes/comments/{}' . format(note_id)
        data = love_comment_data(note_id, comment_id, message_text)
        r = s.post(cfg['basic_url'] + url, 
            data = data,
            headers = headers)
    except Exception as e:
        print(str(e))

def process_comment(notes, cfg, tg_message):
    message_text = tg_message['text']
    if message_text != None:
        from_user = tg_message['from_user']
        reply_to_message = tg_message['reply_to_message']
        if from_user != None and reply_to_message != None:
            if reply_to_message['forward_from_message_id'] != None:
                note_id, comment_id = noteid_by_tgid(notes, reply_to_message['forward_from_message_id'])             
            else:
                note_id, comment_id, comment_author = comid_by_tgid(notes, reply_to_message['message_id'])
                message_text = '{}, {}' . format(comment_author, message_text)
            if comment_id != None:
                send_love_comment(cfg, from_user['id'], note_id, comment_id, message_text)

def process_note(cfg, tg_message):
    message_text = tg_message['text']
    if message_text != None:
        try:
            session_file_name = 'sessions/{}.cookie' . format(cfg['default_tg_userid_session'])
            s = get_user_session(session_file_name)
            url = ''
            data = ''
            r = s.post(cfg['basic_url'] + url, 
                data = data,
                headers = headers)
        except Exception as e:
            print(str(e))

def process_tg_updates(bot, notes, cfg, tg_vars):
    tg_wait(tg_vars)
    updates = get_tg_updates(bot, notes, tg_vars['tg_update_offset'])
    if updates == None:
        return
    while len(updates) > 0:
        for update in updates:
            tg_vars['tg_update_offset'] = update['update_id'] + 1
            tg_message = update['message']
            if tg_message == None:
                continue
            effective_chat_id = update['effective_chat']['id']
            if effective_chat_id == cfg['tg_discussion_chat_id']:
                save_forward_id(notes, tg_message)
                process_comment(notes, cfg, tg_message)
        tg_wait(tg_vars)
        updates = get_tg_updates(bot, notes, tg_vars['tg_update_offset'])
    
#Конец области

def main():
    notes = load_notes()
    cfg   = load_config()
    sbscrbs = load_subscribers()
    tg_vars = {
        "tg_users_states": [],
        "tg_update_offset": 0,
        "tg_last_update_date": datetime.now(),
        "tg_last_post_date": datetime.now()
    }
    bot = telegram.Bot(token = cfg['tg_token'])

    while True:
        crawl_notes(bot, notes, cfg)
        process_tg_updates(bot, 
            notes, 
            cfg, 
            tg_vars)
        crawl_comments(bot, notes, cfg, tg_vars, sbscrbs) 

signal.signal(signal.SIGINT, signal_handler)

if __name__ == '__main__':
    main()
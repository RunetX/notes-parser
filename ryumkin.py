# -*- coding: utf-8 -*-
from datetime import datetime
import json
import os.path
import pickle
import requests
import signal
import telegram
import time

interrupted = False

headers = {
    'User-Agent': 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36'
}

def signal_handler(signal, frame):
    global interrupted
    interrupted = True

def load_json_cfg(file_name):
    try:
        with open(file_name, encoding='utf-8') as json_file:
            data = json.load(json_file)
        return data
    except Exception as e:
        print(str(e))

def load_config():
    return load_json_cfg('ryumkin.json')

def warm_exit(e = None):
    if e != None:
        print(str(e))
    exit()

def tg_wait(tg_vars):
    now = datetime.now()
    time_delta = now - tg_vars['tg_last_update_date']
    delta_seconds = time_delta.total_seconds()
    if delta_seconds < 15:
        time.sleep(15 - delta_seconds)
    tg_vars['tg_last_update_date'] = datetime.now()

def get_tg_updates(bot, tg_update_offset):
    updates = []
    try:
        updates = bot.get_updates(tg_update_offset)
    except Exception as e:
        print(str(e))
    return updates

def process_private_message(bot, cfg, message, tg_users_states):
    message_text = message['text']
    if message_text == None:
        return
    chat_id = message['chat_id']
    if not any(d['id'] == chat_id for d in tg_users_states):
        if message_text.find('/start') != -1:
            process_start_message(bot, chat_id)
            return
        if message_text.find('/login') != -1:
            process_login_message(bot, chat_id)
            tg_user_state = {'id':chat_id, 'state': 1}
            tg_users_states.append(tg_user_state)
        if message_text.find('/add_note') != -1:
            process_addnote_message(bot, chat_id)
            tg_user_state = {'id':chat_id, 'state': 2}
            tg_users_states.append(tg_user_state)
        if message_text.find('/add_anonymous_note') != -1:
            process_addnote_message(bot, chat_id)
            tg_user_state = {'id':chat_id, 'state': 3}
            tg_users_states.append(tg_user_state)
        if message_text.find('/status') != -1:
            get_status(bot, chat_id)
    else:
        tg_user_state = next(item for item in tg_users_states if item["id"] == chat_id)
        if tg_user_state != None:
            state = tg_user_state['state']
            if state == 1:
                result_code = try_login(bot, cfg, chat_id, message_text)
            if state == 2 or state == 3:
                if state == 2:
                    is_anonymous = False
                else:
                    is_anonymous = True
                result_code = add_note(bot, cfg, chat_id, message_text, is_anonymous)
            if result_code == 0:
                tg_users_states.remove(tg_user_state)

def process_start_message(bot, chat_id):
    start_message = """Привет! Меня зовут РюмкинЪ. Пока умею немного:\n
    * /login - войти на сайт НГС.Лав
    * /add_note - добавить заметку
    * /add_anonymous_note - добавить анонимку
    * /status - проверка сессии сайта (необходима для написания)"""
    send_tg_message(bot, chat_id, start_message)

def process_login_message(bot, chat_id):
    login_message = 'Для входа на сайт отправьте логин и пароль через пробел'
    send_tg_message(bot, chat_id, login_message)

def try_login(bot, cfg, chat_id, message_text):
    parts = message_text.split()
    session = requests.session()
    if len(parts) == 2:
        with session as s:
            pload = {'login':parts[0],'password': parts[1]}
            r = s.post(cfg['basic_url'] + '/ajax?request=login', 
                data = pload, 
                headers = headers)
            if r.status_code == 200:
                r_obj = json.loads(r.text)
                if r_obj['login']['result']:
                    with open('sessions/' + str(chat_id) + '.cookie', 'wb') as f:
                        pickle.dump(session.cookies, f)
                    send_tg_message(bot, chat_id, 'Успешный вход. Попробуйте отправить сообщение')
                    return 0
                else:
                    send_tg_message(bot, chat_id, r_obj['login']['errors'])
                    return 2
            else:
                send_tg_message(bot, chat_id, 'Сервер вернул ответ, отличный от успешного ' + str(r.status_code))
                return 3
    else:
        send_tg_message(bot, chat_id, 'Неверное количество параметров. Попробуйте ещё раз')
        return 1

def check_session(tg_user_id):
    session_file_name = 'sessions/{}.cookie' . format(tg_user_id)
    return os.path.exists(session_file_name)

def get_user_session(tg_user_id):
    session = requests.session()  # or an existing session
    session_file_name = 'sessions/{}.cookie' . format(tg_user_id)
    with open(session_file_name, 'rb') as f:
        session.cookies.update(pickle.load(f))
    return session

def get_status(bot, chat_id):
    session_exists = check_session(chat_id)
    if session_exists:
        message = 'Сессия найдена. Попробуйте отправку на сайт. Если не выходит, сделайте повторно /login'
    else:
        message = 'Сессия не найдена. Для отправки на сайт сделайте /login'
    send_tg_message(bot, chat_id, message)

def process_addnote_message(bot, chat_id):
    message = 'Отправьте текст заметки'
    send_tg_message(bot, chat_id, message)

def love_note_data(message_text, is_anonymous):
    if is_anonymous:
        hide_me = 1
    else:
        hide_me = 0
    return {        
        'action_note[lid]': 0,
        'action_note[href]': '',
        'action_note[hideme]': hide_me,
        'action_note[nocom]': 0,
        'action_note[rules]': 1,        
        'id': '',
        'category_note': 0,
        'letter': message_text
    }

def add_note(bot, cfg, chat_id, message_text, is_anonymous):
    try:
        session_exists = check_session(chat_id)
        if not session_exists:
            send_tg_message(bot, chat_id, 'Сессия не найдена. Для отправки на сайт сделайте /login')
            return 1
        s = get_user_session(chat_id)
        url = '/notes/add/'
        data = love_note_data(message_text, is_anonymous)
        r = s.post(cfg['basic_url'] + url, 
            data = data,
            headers = headers)
        return 0
    except Exception as e:
        send_tg_message(bot, chat_id, str(e))
        return 2

def process_tg_updates(bot, cfg, tg_vars):
    tg_wait(tg_vars)
    updates = get_tg_updates(bot, tg_vars['tg_update_offset'])
    while len(updates) > 0:
        for update in updates:
            tg_vars['tg_update_offset'] = update['update_id'] + 1
            tg_message = update['message']
            if tg_message == None:
                continue
            effective_chat_type = update['effective_chat']['type']
            if effective_chat_type == 'private':
                process_private_message(bot, cfg, tg_message, tg_vars['tg_users_states'])
        tg_wait(tg_vars)
        updates = get_tg_updates(bot, tg_vars['tg_update_offset'])

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

def main():

    cfg   = load_config()
    tg_vars = {
        "tg_users_states": [],
        "tg_update_offset": 0,
        "tg_last_update_date": datetime.now(),
        "tg_last_post_date": datetime.now()
    }
    bot = telegram.Bot(token = cfg['tg_token'])

    while not interrupted:
        process_tg_updates(bot, 
            cfg, 
            tg_vars)

signal.signal(signal.SIGINT, signal_handler)

if __name__ == '__main__':
    main()
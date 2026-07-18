# -*- coding: utf-8 -*-
"""Одноразовый экспорт pickle-кук старой версии в JSON для lovegw import.

Запускать на окружении, где работала Python-версия:
    python tools/export_sessions.py [каталог_sessions]

Создаёт sessions_export.json (файл в .gitignore — содержит живые сессии,
не коммитить и удалить после импорта).
"""
import json
import os
import sys


def main():
    sessions_dir = sys.argv[1] if len(sys.argv) > 1 else 'sessions'
    out = {}
    for file_name in sorted(os.listdir(sessions_dir)):
        if not file_name.endswith('.cookie'):
            continue
        uid = file_name[:-len('.cookie')]
        path = os.path.join(sessions_dir, file_name)
        try:
            import pickle
            with open(path, 'rb') as f:
                jar = pickle.load(f)
            out[uid] = [{
                'name': c.name,
                'value': c.value,
                'domain': c.domain,
                'path': c.path,
                'expires': c.expires or 0,
                'secure': bool(c.secure),
            } for c in jar]
        except Exception as e:
            # Битая сессия не блокирует экспорт: пользователь повторит /login
            print(f'ПРОПУЩЕН {file_name}: {e}', file=sys.stderr)
    with open('sessions_export.json', 'w', encoding='utf-8') as f:
        json.dump(out, f, ensure_ascii=False, indent=1)
    print(f'экспортировано сессий: {len(out)} -> sessions_export.json')


if __name__ == '__main__':
    main()

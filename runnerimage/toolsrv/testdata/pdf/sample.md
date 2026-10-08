# Quarterly security review: Acme Logistics

Acme Logistics
14 Harbour Road
Rotterdam 3011 AA

**To:** Dana Okafor, Head of Operations
**From:** The review team
**Date:** 8 October 2026
**Subject:** Findings and next steps

Dear Dana,

This report covers the third quarter. It is written to be read once, start to
finish, and then kept as the record of what was found and what was agreed.

## Summary

- Twelve services were reviewed; nine passed without findings.
- Three had findings, one of them **high** severity, now fixed.
- One recommendation remains open, with an owner and a date.

> A review says what was checked and what was found. It never says that a
> system is safe.

## Findings

| Service | Severity | Finding | Status | Owner | Due |
|---|---|---|---|---|---|
| billing-gateway | high | TLS 1.0 still accepted on the legacy port 8443 | fixed | platform team | 2026-09-30 |
| route-planner | medium | an admin page reachable without a second factor | in progress | identity team | 2026-10-31 |
| warehouse-sync | low | verbose errors expose the framework's version string | accepted | warehouse team | — |

### The configuration that was changed

```nginx
ssl_protocols TLSv1.2 TLSv1.3; ssl_ciphers 'ECDHE-ECDSA-AES256-GCM-SHA384:ECDHE-RSA-AES256-GCM-SHA384:ECDHE-ECDSA-CHACHA20-POLY1305';
```

The deployed configuration's digest is
sha256:9f2c4e7a1b3d5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f, and the
change is recorded at https://example.com/changes/2026/q3/billing-gateway/tls-hardening-and-cipher-review-follow-up.

## In the languages of the teams

- 中文：第三季度的审查已经完成，所有发现都有负责人。
- 日本語：第3四半期のレビューが完了しました。
- 한국어: 3분기 검토가 완료되었습니다.
- العربية: اكتملت مراجعة الربع الثالث، ولكل نتيجة مسؤول.
- עברית: סקירת הרבעון השלישי הושלמה.
- हिन्दी: तीसरी तिमाही की समीक्षा पूरी हो गई है।
- বাংলা: তৃতীয় প্রান্তিকের পর্যালোচনা সম্পন্ন হয়েছে।
- தமிழ்: மூன்றாம் காலாண்டு மதிப்பாய்வு நிறைவடைந்தது.
- ไทย: การตรวจสอบไตรมาสที่สามเสร็จสมบูรณ์แล้ว
- Ελληνικά: Η αναθεώρηση του τρίτου τριμήνου ολοκληρώθηκε.
- Русский: Проверка третьего квартала завершена.
- Status: ✅ done, 🔒 locked, ⚠️ one open item

## Next steps

1. The identity team closes the second-factor gap by 31 October.
2. The review team checks the fix and updates this record.
3. The next review starts in January.

Kind regards,

The review team
Acme Logistics

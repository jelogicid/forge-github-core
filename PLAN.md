# github-core-clone-01 — rencana build

## Fakta terukur (dari kit + server)
- Tugas: git forge standalone SATU proses (Node22/Python3.13/stdlib+vendor; boleh static ELF Go).
  Surface: git smart HTTP v2 (/{o}/{r}.git), REST v3 (/api/v3), GraphQL (/api/graphql), MCP (/mcp).
- Skor 0..1000 = feature(600, 120 checks) + agent(200, 15 MCP) + perf(200, 4 workload).
  Grup checks: git 30, repos 15, issues 15, pulls 20, graphql 20, mcp 15, protocol 5 = 120.
- T_ref (ms): push 1640.82, clone 266.94, read 2759.72, write 348.97.
  perf/workload = round(clamp(25 - 12.5*log2(t/T_ref),0,50)).
- Skenario NAMES tetap (buildScenario pakai SCENARIO_SEED konstan, env main.ts panggil tanpa arg):
  viewer.login=adaiversen, stranger=olgabauer, teammate=quinnkeller, org=faint-meadow-org.
  Token Bearer apa pun -> owner adaiversen; owner lain -> 404; tanpa token -> 401.
- Batas: 16 MiB total source, 8 MiB/file; 2 CPU/1024MB/900s; deterministik (state export byte-identik).
- Repo publik milik jelogicid, dibuat setelah opening 2026-11-14T06:30Z (sudah lewat), lisensi OSI,
  main.ts/main.py di root (Go: main.go dikompilasi jadi binary, entrypoint main.ts wrapper? -> cek gate).
- Deadline 2026-11-21T06:30Z. Final 06:40..18:40Z. <=5 submit/hari, <=10 total.
- Prize harus kalahkan baseline house (90% paired t-interval lead>0). 20 tempat $500.

## Kondisi kompetisi (leaderboard live)
- 9071 entry. Top: 970/966/965... semua feature 600 + agent 200 + perf 160-170.
  Mayoritas pemimpin pakai "Static Go Core" + zero-alloc hot paths. Bar top-20 ~955+.
- Kesimpulan jujur: ini bounty TERBERAT. Butuh forge Go lengkap lolos 120 checks + perf tinggi.

## Strategi
- Go static binary (go1.23.2 ada) -> perf setara pemimpin. gcc ada sebagai cadangan.
- Build bertahap, ukur tiap fase pakai sdk/run.ts (suite sealed, byte-identik):
  Fase 1: lifecycle + protocol(5) + repos(15) + issues(15) + pulls(20) = 55 checks.
  Fase 2: graphql(20) + mcp(15) = +35 -> 90 checks, feature ~450 + agent 200.
  Fase 3: git smart HTTP(30) -> 120 checks, feature 600.
  Fase 4: optimasi perf (push/clone/read/write) -> target perf 150+.
- Verifikasi: node --experimental-strip-types sdk/run.ts --url http://127.0.0.1:PORT --token TOK --perf --json.
- Entry repo: work/github-core-clone-01-entry. main.ts lifecycle wrapper + binary Go ter-vendor.

## UNKNOWN / risiko
- Gate entrypoint: rules minta main.ts/main.py di root. Go binary perlu wrapper main.ts yang spawn binary?
  Atau main.go diterima? -> cek makefun check / rules entrypoint. (Node22 runtime; Go binary static ELF legal sebagai vendor.)
- Git protocol v2 server-side: bagian tersulit; perlu packfile parse/build + ref negotiation.
- Final seed: nama skenario tetap (terkonfirmasi), tapi git fixture mungkin pakai instance seed -> uji.
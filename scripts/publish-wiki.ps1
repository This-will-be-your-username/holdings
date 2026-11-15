<#
Publishes docs/wiki to the repository's GitHub wiki.

GitHub wikis are only available for public repositories, or private ones on a paid plan. Enable "Wikis" in the repository
settings; if the wiki repository does not exist yet, create the first page once in the browser. Then:

    ./scripts/publish-wiki.ps1 -Repo owner/holdings

Pages are copied with links rewritten from "Page.md" (which works when browsing the repository) to "Page" (which is how
GitHub wikis link between pages).
#>
param([Parameter(Mandatory = $true)][string]$Repo)

# git writes progress to stderr, which Windows PowerShell treats as an error, so check exit codes instead.
$ErrorActionPreference = 'Continue'
function Run([string]$what, [scriptblock]$cmd) {
    & $cmd 2>&1 | ForEach-Object { "$_" } | Write-Host
    if ($LASTEXITCODE -ne 0) { throw "$what failed (exit code $LASTEXITCODE)" }
}

$root = Split-Path -Parent $PSScriptRoot
$src = Join-Path $root 'docs\wiki'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("holdings-wiki-" + [guid]::NewGuid().ToString('N'))
Run 'clone' { git clone "https://github.com/$Repo.wiki.git" $tmp }
$pages = Get-ChildItem $src -Filter *.md | ForEach-Object { $_.BaseName }
foreach ($f in Get-ChildItem $src -Filter *.md) {
    $c = [IO.File]::ReadAllText($f.FullName)
    $c = [regex]::Replace($c, '\]\(([A-Za-z0-9\-]+)\.md\)', { param($m) if ($pages -contains $m.Groups[1].Value) { "]($($m.Groups[1].Value))" } else { $m.Value } })
    [IO.File]::WriteAllText((Join-Path $tmp $f.Name), $c, (New-Object Text.UTF8Encoding $false))
}
Copy-Item (Join-Path $src 'images') $tmp -Recurse -Force
Push-Location $tmp
try {
    Run 'add' { git add -A }
    git diff --cached --quiet
    if ($LASTEXITCODE -eq 0) { Write-Host 'Wiki is already up to date.' }
    else {
        Run 'commit' { git -c user.name="FundTrack Dev" -c user.email="dev@example.invalid" commit -m "Update wiki from docs/wiki" }
        Run 'push' { git push }
    }
}
finally { Pop-Location; Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue }

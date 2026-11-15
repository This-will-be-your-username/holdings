<#
Publishes docs/wiki to the repository's GitHub wiki.

GitHub wikis are only available for public repositories, or private ones on a paid plan. Enable "Wikis" in the repository
settings and create the first page once in the browser (GitHub only creates the wiki repository then); after that:

    ./scripts/publish-wiki.ps1 -Repo owner/fund-track

Pages are copied with links rewritten from "Page.md" (which works when browsing the repository) to "Page" (which is how
GitHub wikis link between pages).
#>
param([Parameter(Mandatory = $true)][string]$Repo)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$src = Join-Path $root 'docs\wiki'
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("fundtrack-wiki-" + [guid]::NewGuid().ToString('N'))
git clone "https://github.com/$Repo.wiki.git" $tmp
$pages = Get-ChildItem $src -Filter *.md | ForEach-Object { $_.BaseName }
foreach ($f in Get-ChildItem $src -Filter *.md) {
    $c = [IO.File]::ReadAllText($f.FullName)
    $c = [regex]::Replace($c, '\]\(([A-Za-z0-9\-]+)\.md\)', { param($m) if ($pages -contains $m.Groups[1].Value) { "]($($m.Groups[1].Value))" } else { $m.Value } })
    [IO.File]::WriteAllText((Join-Path $tmp $f.Name), $c, (New-Object Text.UTF8Encoding $false))
}
Copy-Item (Join-Path $src 'images') $tmp -Recurse -Force
Push-Location $tmp
git add -A
git -c user.name="FundTrack Dev" -c user.email="dev@example.invalid" commit -m "Update wiki from docs/wiki"
git push
Pop-Location
Remove-Item -Recurse -Force $tmp

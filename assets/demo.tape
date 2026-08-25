# VHS Tape Script: assets/demo.tape
Output assets/demo.gif

Set FontSize 15
Set Width 1200
Set Height 650
Set Padding 20
Set Theme "Catppuccin Mocha"

Type "polyql translate --from promql --to logql --query 'up'"
Sleep 500ms
Enter

Sleep 3.5s

Type "polyql dashboard translate --from promql --to logql --input dashboard.json"
Sleep 500ms
Enter

Sleep 3s

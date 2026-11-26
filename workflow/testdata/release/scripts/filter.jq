{paths: [.files[].path | select(endswith(".go"))], added: ([.files[].additions] | add)}

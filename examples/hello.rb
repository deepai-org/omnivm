require 'json'

data = { language: "Ruby", version: RUBY_VERSION }
puts JSON.generate(data)

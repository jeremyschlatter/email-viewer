require 'sanitize'

puts Sanitize.document(STDIN.read, Sanitize::Config.merge(Sanitize::Config::RELAXED,
                                                          :remove_contents => true))

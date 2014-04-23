<?
require 'htmlpurifier-4.6.0-standalone/HTMLPurifier.standalone.php';

$purifier = new HTMLPurifier(); 
echo($purifier->purify(stream_get_contents(STDIN)));
?>

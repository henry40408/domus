Feature: Access tokens

  Background:
    Given a domus server with the admin "demo" and the Hue bridge paired
    And I am logged in as "demo"
    And I go to the "Tokens" tab

  Scenario: A new token is shown only once
    When I create the token "Garmin watch"
    Then the page should show the new token
    When I reload the page
    Then the token "Garmin watch" should be listed
    But the page should not show the new token
